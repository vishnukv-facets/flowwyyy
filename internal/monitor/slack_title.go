package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/slack-go/slack"
	"rsc.io/pdf"
)

const (
	slackTitleContextLimit   = 96
	slackFileContentMaxBytes = 64 * 1024
	slackPDFContentMaxBytes  = 2 * 1024 * 1024
)

var errSlackFileContentLimit = errors.New("slack file content limit reached")

// SlackConversation is the small, testable subset of conversations.info data
// needed to name Slack-origin tasks.
type SlackConversation struct {
	ID        string
	Name      string
	User      string
	Members   []string
	IsIM      bool
	IsMpIM    bool
	IsChannel bool
	IsGroup   bool
}

// SlackMessage is the small, testable subset of conversations.replies data
// needed to derive thread context and reconcile missed replies. TS/ThreadTS/
// SubType are populated for the backfill path (see slack_backfill.go); title
// generation only reads User/Text.
type SlackMessage struct {
	User     string
	Text     string
	TS       string
	ThreadTS string
	SubType  string
	Files    []SlackFile
	// Thread metadata, set on the top-level (parent) message by
	// conversations.history. ReplyCount > 0 marks a thread; LatestReply is the
	// ts of its most recent reply. The steering backfill uses these to detect
	// threads that gained replies during downtime (which a top-level history
	// sweep alone never surfaces) and fetch those replies. Zero/"" for replies
	// and for messages that aren't thread parents.
	ReplyCount  int
	LatestReply string
}

// SlackFile is the file metadata needed to make file-only Slack messages
// visible to the steerer without exposing private download URLs.
type SlackFile struct {
	ID                 string
	Name               string
	Title              string
	Mimetype           string
	Filetype           string
	PrettyType         string
	Size               int
	URLPrivate         string `json:"-"`
	URLPrivateDownload string `json:"-"`
	Content            string
	ContentTruncated   bool
	SecurityReport     string
	LocalPath          string
}

// DisplayText returns the message body, falling back to file titles for
// file-only messages.
func (m SlackMessage) DisplayText() string {
	return slackMessageDisplayText(m.Text, m.Files)
}

func slackFilesFromAPI(files []slack.File) []SlackFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]SlackFile, 0, len(files))
	for _, f := range files {
		sf := SlackFile{
			ID:                 strings.TrimSpace(f.ID),
			Name:               strings.TrimSpace(f.Name),
			Title:              strings.TrimSpace(f.Title),
			Mimetype:           strings.TrimSpace(f.Mimetype),
			Filetype:           strings.TrimSpace(f.Filetype),
			PrettyType:         strings.TrimSpace(f.PrettyType),
			Size:               f.Size,
			URLPrivate:         strings.TrimSpace(f.URLPrivate),
			URLPrivateDownload: strings.TrimSpace(f.URLPrivateDownload),
			Content:            strings.TrimSpace(f.Preview),
			ContentTruncated:   f.LinesMore > 0,
		}
		if sf.Name == "" && sf.Title == "" && sf.Mimetype == "" && sf.Filetype == "" && sf.PrettyType == "" && sf.Content == "" {
			continue
		}
		out = append(out, sf)
	}
	return out
}

var slackFileDownloadFn = func(ctx context.Context, api *slack.Client, downloadURL string, maxBytes int) ([]byte, bool, error) {
	if api == nil {
		return nil, false, errors.New("slack api client unavailable")
	}
	downloadURL = strings.TrimSpace(downloadURL)
	if downloadURL == "" {
		return nil, false, errors.New("slack file download URL unavailable")
	}
	if maxBytes <= 0 {
		maxBytes = slackFileContentMaxBytes
	}
	var buf limitedSlackFileBuffer
	buf.limit = maxBytes
	err := api.GetFileContext(ctx, downloadURL, &buf)
	if err != nil && !errors.Is(err, errSlackFileContentLimit) {
		return nil, false, err
	}
	return append([]byte(nil), buf.buf.Bytes()...), buf.truncated, nil
}

type SlackImageFileSaver func(ctx context.Context, channel string, file SlackFile, data []byte) (string, error)

var slackImageSaver = struct {
	sync.Mutex
	fn       SlackImageFileSaver
	maxBytes int
}{}

func SetSlackImageFileSaver(fn SlackImageFileSaver, maxBytes int) func() {
	slackImageSaver.Lock()
	oldFn, oldMax := slackImageSaver.fn, slackImageSaver.maxBytes
	slackImageSaver.fn, slackImageSaver.maxBytes = fn, maxBytes
	slackImageSaver.Unlock()
	return func() {
		slackImageSaver.Lock()
		slackImageSaver.fn, slackImageSaver.maxBytes = oldFn, oldMax
		slackImageSaver.Unlock()
	}
}

func currentSlackImageFileSaver() (SlackImageFileSaver, int) {
	slackImageSaver.Lock()
	defer slackImageSaver.Unlock()
	return slackImageSaver.fn, slackImageSaver.maxBytes
}

type limitedSlackFileBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedSlackFileBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.truncated = true
		return 0, errSlackFileContentLimit
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return 0, errSlackFileContentLimit
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return remaining, errSlackFileContentLimit
	}
	return b.buf.Write(p)
}

func (b *limitedSlackFileBuffer) String() string {
	if b == nil {
		return ""
	}
	return b.buf.String()
}

func slackFilesFromAPIWithContent(ctx context.Context, api *slack.Client, channel string, files []slack.File) []SlackFile {
	out := slackFilesFromAPI(files)
	for i := range out {
		if out[i].isImageLike() {
			if !saveSlackImageFile(ctx, api, channel, &out[i]) && strings.TrimSpace(out[i].SecurityReport) == "" {
				out[i].SecurityReport = "Security report: content not fetched; image download unavailable for model attachment."
			}
			continue
		}
		if !out[i].isReadableBySafeExtractor() {
			out[i].SecurityReport = "Security report: content not fetched; unsupported file type for safe text extraction."
			continue
		}
		maxBytes := out[i].downloadMaxBytes()
		if out[i].Size > maxBytes && strings.TrimSpace(out[i].Content) == "" {
			out[i].SecurityReport = fmt.Sprintf("Security report: content not fetched; file size %d bytes exceeds safe fetch limit %d bytes.", out[i].Size, maxBytes)
			continue
		}
		url := firstNonEmpty(out[i].URLPrivateDownload, out[i].URLPrivate)
		if url != "" {
			data, downloadTruncated, err := slackFileDownloadFn(ctx, api, url, maxBytes)
			if err == nil && len(data) > 0 {
				if slackDownloadedContentLooksHTML(data) {
					out[i].SecurityReport = "Security report: content not scanned; Slack returned an HTML page instead of file bytes. Reinstall Slack with files:read scope if this persists."
				} else if out[i].isPDFLike() {
					content, extractTruncated, extractErr := slackPDFExtractTextFn(data, slackFileContentMaxBytes)
					if extractErr == nil && strings.TrimSpace(content) != "" {
						out[i].Content = content
						out[i].ContentTruncated = downloadTruncated || extractTruncated
					}
				} else {
					out[i].Content = strings.TrimSpace(string(data))
					out[i].ContentTruncated = downloadTruncated
				}
			}
		}
		if strings.TrimSpace(out[i].Content) == "" {
			if strings.TrimSpace(out[i].SecurityReport) != "" {
				continue
			}
			if out[i].isPDFLike() {
				out[i].SecurityReport = "Security report: content not scanned; PDF had no extractable text or extraction failed."
			} else {
				out[i].SecurityReport = "Security report: content not scanned; readable file content was unavailable."
			}
			continue
		}
		out[i].SecurityReport = slackFileSecurityReport(out[i].Content, out[i].isPDFLike())
	}
	return out
}

func saveSlackImageFile(ctx context.Context, api *slack.Client, channel string, file *SlackFile) bool {
	if file == nil {
		return false
	}
	saveFn, maxBytes := currentSlackImageFileSaver()
	if saveFn == nil {
		return false
	}
	if maxBytes <= 0 {
		maxBytes = slackFileContentMaxBytes
	}
	if file.Size > maxBytes {
		file.SecurityReport = fmt.Sprintf("Security report: image not fetched; file size %d bytes exceeds attachment limit %d bytes.", file.Size, maxBytes)
		return false
	}
	url := firstNonEmpty(file.URLPrivateDownload, file.URLPrivate)
	if url == "" {
		file.SecurityReport = "Security report: image not fetched; Slack file download URL unavailable."
		return false
	}
	data, downloadTruncated, err := slackFileDownloadFn(ctx, api, url, maxBytes)
	if err != nil || len(data) == 0 {
		if err != nil {
			file.SecurityReport = "Security report: image not fetched; " + err.Error()
		}
		return false
	}
	if downloadTruncated {
		file.SecurityReport = fmt.Sprintf("Security report: image not attached; file exceeds attachment limit %d bytes.", maxBytes)
		return false
	}
	if slackDownloadedContentLooksHTML(data) {
		file.SecurityReport = "Security report: image not attached; Slack returned an HTML page instead of file bytes. Reinstall Slack with files:read scope if this persists."
		return false
	}
	if detected, ok := safeRasterImageContentType(data); !ok {
		file.SecurityReport = fmt.Sprintf("Security report: image not attached; downloaded content type %q is not a verified raster image.", detected)
		return false
	}
	path, err := saveFn(ctx, strings.TrimSpace(channel), *file, data)
	if err != nil || strings.TrimSpace(path) == "" {
		if err != nil {
			file.SecurityReport = "Security report: image not saved for model attachment; " + err.Error()
		}
		return false
	}
	file.LocalPath = strings.TrimSpace(path)
	return true
}

var slackPDFExtractTextFn = extractSlackPDFText

func extractSlackPDFText(data []byte, maxChars int) (text string, truncated bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, truncated, err = "", false, fmt.Errorf("pdf text extraction failed: %v", r)
		}
	}()
	if len(data) == 0 {
		return "", false, errors.New("empty pdf")
	}
	if maxChars <= 0 {
		maxChars = slackFileContentMaxBytes
	}
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	for pageNo := 1; pageNo <= r.NumPage(); pageNo++ {
		pageText := extractSlackPDFPageText(r.Page(pageNo))
		if strings.TrimSpace(pageText) == "" {
			continue
		}
		if b.Len() > 0 {
			if b.Len()+2 > maxChars {
				return b.String(), true, nil
			}
			b.WriteString("\n\n")
		}
		remaining := maxChars - b.Len()
		if len(pageText) > remaining {
			b.WriteString(pageText[:remaining])
			return b.String(), true, nil
		}
		b.WriteString(pageText)
	}
	return strings.TrimSpace(b.String()), false, nil
}

func extractSlackPDFPageText(page pdf.Page) string {
	content := page.Content()
	if len(content.Text) == 0 {
		return ""
	}
	text := append([]pdf.Text(nil), content.Text...)
	sort.Sort(pdf.TextVertical(text))
	var b strings.Builder
	var lastY, lastX, lastW float64
	haveLast := false
	for _, item := range text {
		s := strings.TrimSpace(item.S)
		if s == "" {
			continue
		}
		if haveLast {
			if math.Abs(item.Y-lastY) > 2 {
				b.WriteByte('\n')
			} else if item.X > lastX+math.Max(lastW, 3) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(s)
		lastY, lastX, lastW = item.Y, item.X, item.W
		haveLast = true
	}
	return strings.TrimSpace(b.String())
}

func slackDownloadedContentLooksHTML(data []byte) bool {
	s := strings.TrimSpace(strings.ToLower(string(data[:min(len(data), 512)])))
	return strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html") || strings.Contains(s, "<title>slack")
}

func slackMessageDisplayText(text string, files []SlackFile) string {
	text = strings.TrimSpace(text)
	if text != "" {
		parts := []string{text}
		for _, f := range files {
			if fileText := f.displayText(); fileText != "" {
				parts = append(parts, fileText)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	if len(files) == 0 {
		return ""
	}
	parts := make([]string, 0, len(files))
	for _, f := range files {
		if fileText := f.displayText(); fileText != "" {
			parts = append(parts, fileText)
		}
	}
	return strings.Join(parts, "\n")
}

func (f SlackFile) displayText() string {
	label := firstNonEmpty(strings.TrimSpace(f.Title), strings.TrimSpace(f.Name), "untitled file")
	kind := firstNonEmpty(strings.TrimSpace(f.PrettyType), strings.TrimSpace(f.Filetype), strings.TrimSpace(f.Mimetype))
	header := "file: " + label
	if kind != "" {
		header += " (" + kind + ")"
	}
	content := strings.TrimSpace(f.Content)
	report := strings.TrimSpace(f.SecurityReport)
	localPath := strings.TrimSpace(f.LocalPath)
	if content == "" {
		if localPath != "" {
			return header + "\n\nImage attachment saved for model vision: " + localPath
		}
		if report != "" {
			return header + "\n\n" + report
		}
		return header
	}
	if f.ContentTruncated {
		content += "\n[truncated]"
	}
	if report != "" {
		content += "\n\n" + report
	}
	return header + "\n\n" + content
}

func (f SlackFile) isReadableBySafeExtractor() bool {
	return f.isTextLike() || f.isPDFLike()
}

func (f SlackFile) isPDFLike() bool {
	mime := strings.ToLower(strings.TrimSpace(f.Mimetype))
	filetype := strings.ToLower(strings.TrimSpace(f.Filetype))
	pretty := strings.ToLower(strings.TrimSpace(f.PrettyType))
	name := strings.ToLower(firstNonEmpty(f.Name, f.Title))
	return mime == "application/pdf" || filetype == "pdf" || strings.Contains(pretty, "pdf") || strings.HasSuffix(name, ".pdf")
}

func (f SlackFile) isImageLike() bool {
	if f.isSVGLike() {
		return false
	}
	mime := strings.ToLower(strings.TrimSpace(f.Mimetype))
	if strings.HasPrefix(mime, "image/") {
		return true
	}
	filetype := strings.ToLower(strings.TrimSpace(f.Filetype))
	pretty := strings.ToLower(strings.TrimSpace(f.PrettyType))
	switch filetype {
	case "png", "jpg", "jpeg", "gif", "webp", "heic", "heif", "tiff", "bmp":
		return true
	}
	if strings.Contains(pretty, "image") || strings.Contains(pretty, "png") || strings.Contains(pretty, "jpeg") || strings.Contains(pretty, "gif") {
		return true
	}
	name := strings.ToLower(firstNonEmpty(f.Name, f.Title))
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic", ".heif", ".tif", ".tiff", ".bmp"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

func (f SlackFile) isSVGLike() bool {
	mime := strings.ToLower(strings.TrimSpace(f.Mimetype))
	filetype := strings.ToLower(strings.TrimSpace(f.Filetype))
	pretty := strings.ToLower(strings.TrimSpace(f.PrettyType))
	name := strings.ToLower(firstNonEmpty(f.Name, f.Title))
	return strings.Contains(mime, "svg") ||
		filetype == "svg" ||
		strings.Contains(pretty, "svg") ||
		strings.HasSuffix(name, ".svg")
}

func safeRasterImageContentType(data []byte) (string, bool) {
	detected := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0]))
	switch detected {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return detected, true
	default:
		return detected, false
	}
}

func (f SlackFile) downloadMaxBytes() int {
	if f.isPDFLike() {
		return slackPDFContentMaxBytes
	}
	return slackFileContentMaxBytes
}

func (f SlackFile) isTextLike() bool {
	mime := strings.ToLower(strings.TrimSpace(f.Mimetype))
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/x-yaml", "application/yaml", "application/xml", "application/javascript", "application/x-sh":
		return true
	}
	filetype := strings.ToLower(strings.TrimSpace(f.Filetype))
	pretty := strings.ToLower(strings.TrimSpace(f.PrettyType))
	if strings.Contains(pretty, "markdown") || strings.Contains(pretty, "text") {
		return true
	}
	switch filetype {
	case "text", "txt", "markdown", "md", "post", "json", "yaml", "yml", "xml", "csv", "log", "go", "py", "js", "ts", "tsx", "jsx", "sh", "terraform", "tf":
		return true
	}
	name := strings.ToLower(firstNonEmpty(f.Name, f.Title))
	for _, ext := range []string{".txt", ".md", ".markdown", ".json", ".yaml", ".yml", ".xml", ".csv", ".log", ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".sh", ".tf", ".tfvars"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

func slackFileSecurityReport(content string, pdfSource bool) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return "Security report: content not scanned; no extracted text was available."
	}
	findings := slackFileSecurityFindings(content)
	scope := "fetched content"
	if pdfSource {
		scope = "extracted PDF text"
	}
	if len(findings) == 0 {
		return "Security report: no high-risk code indicators found in " + scope + "."
	}
	return "Security report: potential high-risk code indicators found in " + scope + ":\n- " + strings.Join(findings, "\n- ")
}

type slackFileSecurityPattern struct {
	label string
	re    *regexp.Regexp
}

var slackFileSecurityPatterns = []slackFileSecurityPattern{
	{label: "download-and-execute shell pipeline", re: regexp.MustCompile(`(?is)\b(curl|wget)\b[^\n|;]*\|\s*(sudo\s+)?(sh|bash|zsh|python|python3|perl|ruby)\b`)},
	{label: "destructive filesystem command", re: regexp.MustCompile(`(?is)\b(sudo\s+)?rm\s+-(?:[a-zA-Z]*r[a-zA-Z]*f|[a-zA-Z]*f[a-zA-Z]*r)\s+(/|\$HOME|~|\*)|\bmkfs(?:\.[a-z0-9]+)?\b|\bdd\s+if=.+\s+of=/dev/`)},
	{label: "reverse shell or raw network shell indicator", re: regexp.MustCompile(`(?is)(/dev/tcp/|\bnc\s+[^;\n]*\s-e\s|\bncat\s+[^;\n]*\s-e\s|bash\s+-i\s+>&|python(?:3)?\s+-c\s+['"][^'"]*socket\.)`)},
	{label: "encoded payload execution", re: regexp.MustCompile(`(?is)(base64\s+(-d|--decode)\s*\|\s*(sh|bash|python|python3|perl|ruby)|powershell(?:\.exe)?\s+[^;\n]*(?:-enc|-encodedcommand)|frombase64string\s*\()`)},
	{label: "embedded private key or credential material", re: regexp.MustCompile(`(?is)(-----BEGIN [A-Z ]*PRIVATE KEY-----|AWS_SECRET_ACCESS_KEY|SLACK_[A-Z_]*TOKEN|xox[baprs]-[A-Za-z0-9-]+|gh[pousr]_[A-Za-z0-9_]{20,}|Authorization:\s*Bearer\s+[A-Za-z0-9._~+/=-]+)`)},
	{label: "persistence or privileged startup modification", re: regexp.MustCompile(`(?is)(\bcrontab\s+|/etc/cron\.|systemctl\s+enable|launchctl\s+(load|bootstrap)|chmod\s+\+x\s+[^;\n]+&&\s*[^;\n]+)`)},
}

func slackFileSecurityFindings(content string) []string {
	var findings []string
	for _, pattern := range slackFileSecurityPatterns {
		if pattern.re.MatchString(content) {
			findings = append(findings, pattern.label)
		}
	}
	return findings
}

// SlackUser is the small, testable subset of users.info data needed for
// human-readable participant names.
type SlackUser struct {
	ID          string
	Name        string
	RealName    string
	DisplayName string
}

// SlackTitleClient hides slack-go behind the exact read calls title generation
// needs. Tests use a fake client; production uses slackTitleAPIClient.
type SlackTitleClient interface {
	ConversationInfo(ctx context.Context, channelID string) (SlackConversation, error)
	ConversationReplies(ctx context.Context, channelID, threadTS string, limit int) ([]SlackMessage, error)
	UsersInConversation(ctx context.Context, channelID string, limit int) ([]string, error)
	UserInfo(ctx context.Context, userID string) (SlackUser, error)
}

type slackTitleAPIClient struct {
	lazy *lazySlackClient
}

func newSlackTitleAPIClient() SlackTitleClient {
	if strings.TrimSpace(SlackBotToken()) == "" {
		return nil
	}
	return slackTitleAPIClient{lazy: newLazySlackClient(SlackBotToken)}
}

// NewSlackTitleUserClient returns a USER-token title client, or nil when no
// user token is configured. The name resolver uses it as a fallback for
// channels the bot can't see (private channels it was never invited to →
// conversations.info returns channel_not_found): the operator's token resolves
// them because the operator is a member. Token resolved per call (lazy), so a
// rotated token is picked up without rebuilding the client.
func NewSlackTitleUserClient() SlackTitleClient {
	if strings.TrimSpace(SlackUserToken()) == "" {
		return nil
	}
	return slackTitleAPIClient{lazy: newLazySlackClient(SlackUserToken)}
}

func (c slackTitleAPIClient) ConversationInfo(ctx context.Context, channelID string) (SlackConversation, error) {
	api := c.lazy.client()
	if api == nil {
		return SlackConversation{}, ErrNoToken
	}
	ch, err := api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
		ChannelID:         normalizeSlackChannelID(channelID),
		IncludeNumMembers: true,
	})
	if err != nil {
		return SlackConversation{}, err
	}
	if ch == nil {
		return SlackConversation{}, errors.New("slack conversation info missing")
	}
	return SlackConversation{
		ID:        firstNonEmpty(ch.ID, normalizeSlackChannelID(channelID)),
		Name:      firstNonEmpty(ch.Name, ch.NameNormalized),
		User:      ch.User,
		Members:   ch.Members,
		IsIM:      ch.IsIM,
		IsMpIM:    ch.IsMpIM,
		IsChannel: ch.IsChannel,
		IsGroup:   ch.IsGroup,
	}, nil
}

func (c slackTitleAPIClient) ConversationReplies(ctx context.Context, channelID, threadTS string, limit int) ([]SlackMessage, error) {
	api := c.lazy.client()
	if api == nil {
		return nil, ErrNoToken
	}
	msgs, _, _, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: normalizeSlackChannelID(channelID),
		Timestamp: strings.TrimSpace(threadTS),
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SlackMessage, 0, len(msgs))
	for _, msg := range msgs {
		files := slackFilesFromAPIWithContent(ctx, api, channelID, msg.Files)
		out = append(out, SlackMessage{
			User:     firstNonEmpty(msg.User, msg.Username),
			Text:     strings.TrimSpace(msg.Text),
			TS:       msg.Timestamp,
			ThreadTS: msg.ThreadTimestamp,
			SubType:  msg.SubType,
			Files:    files,
		})
	}
	return out, nil
}

func (c slackTitleAPIClient) UsersInConversation(ctx context.Context, channelID string, limit int) ([]string, error) {
	api := c.lazy.client()
	if api == nil {
		return nil, ErrNoToken
	}
	members, _, err := api.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{
		ChannelID: normalizeSlackChannelID(channelID),
		Limit:     limit,
	})
	return members, err
}

func (c slackTitleAPIClient) UserInfo(ctx context.Context, userID string) (SlackUser, error) {
	api := c.lazy.client()
	if api == nil {
		return SlackUser{}, ErrNoToken
	}
	u, err := api.GetUserInfoContext(ctx, strings.TrimSpace(userID))
	if err != nil {
		return SlackUser{}, err
	}
	if u == nil {
		return SlackUser{}, errors.New("slack user info missing")
	}
	return SlackUser{
		ID:          u.ID,
		Name:        u.Name,
		RealName:    firstNonEmpty(u.Profile.RealName, u.Profile.RealNameNormalized, u.RealName),
		DisplayName: firstNonEmpty(u.Profile.DisplayName, u.Profile.DisplayNameNormalized),
	}, nil
}

// BuildSlackTaskTitle turns Slack metadata into the human-facing task/session
// title. The slug remains the durable identity; this title is display-only.
func BuildSlackTaskTitle(ctx context.Context, client SlackTitleClient, decision ReactionDecision, selfUserIDs []string) (string, error) {
	if client == nil {
		return "", errors.New("slack title client unavailable")
	}
	channelID := normalizeSlackChannelID(decision.Channel)
	threadTS := strings.TrimSpace(decision.ThreadTS)
	if channelID == "" || threadTS == "" {
		return "", errors.New("slack channel and thread_ts are required")
	}
	var prefix string
	if conversation, err := client.ConversationInfo(ctx, channelID); err == nil {
		prefix = slackTitlePrefix(ctx, client, conversation, channelID, selfUserIDs)
	} else if authorID := strings.TrimSpace(decision.Event.ItemAuthor); authorID != "" {
		// ConversationInfo failed (commonly a missing channels:read /
		// groups:read scope on the token). The thread reply below is
		// fetched independently, so we can still build a useful title
		// from the message author's display name.
		prefix = slackUserDisplayName(ctx, client, authorID)
	}
	if prefix == "" {
		prefix = channelID
	}
	contextText := slackThreadContext(ctx, client, channelID, threadTS, decision.Event.Text)
	if contextText == "" {
		return truncateRunes(prefix, slackTitleContextLimit), nil
	}
	return truncateRunes(prefix+" - "+contextText, slackTitleContextLimit), nil
}

func slackTitlePrefix(ctx context.Context, client SlackTitleClient, conversation SlackConversation, channelID string, selfUserIDs []string) string {
	switch {
	case conversation.IsIM || strings.HasPrefix(channelID, "D"):
		userID := strings.TrimSpace(conversation.User)
		if userID == "" {
			for _, member := range conversation.Members {
				if !containsUserID(selfUserIDs, member) {
					userID = member
					break
				}
			}
		}
		return slackUserDisplayName(ctx, client, userID)
	case conversation.IsMpIM:
		members := conversation.Members
		if len(members) == 0 {
			if got, err := client.UsersInConversation(ctx, channelID, 8); err == nil {
				members = got
			}
		}
		names := make([]string, 0, 3)
		remaining := 0
		for _, member := range members {
			if containsUserID(selfUserIDs, member) {
				continue
			}
			name := slackUserDisplayName(ctx, client, member)
			if name == "" {
				name = member
			}
			if len(names) < 3 {
				names = append(names, name)
			} else {
				remaining++
			}
		}
		if len(names) == 0 {
			return firstNonEmpty(conversation.Name, channelID)
		}
		out := strings.Join(names, ", ")
		if remaining > 0 {
			out += " +" + strconv.Itoa(remaining)
		}
		return out
	default:
		name := firstNonEmpty(conversation.Name, channelID)
		if strings.HasPrefix(name, "#") {
			return name
		}
		if strings.HasPrefix(channelID, "C") || strings.HasPrefix(channelID, "G") {
			return "#" + strings.TrimPrefix(name, "#")
		}
		return name
	}
}

func slackThreadContext(ctx context.Context, client SlackTitleClient, channelID, threadTS, fallback string) string {
	msgs, err := client.ConversationReplies(ctx, channelID, threadTS, 1)
	if err == nil {
		for _, msg := range msgs {
			if text := cleanSlackTitleText(msg.DisplayText()); text != "" {
				return text
			}
		}
	}
	return cleanSlackTitleText(fallback)
}

func slackUserDisplayName(ctx context.Context, client SlackTitleClient, userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	user, err := client.UserInfo(ctx, userID)
	if err != nil {
		return userID
	}
	return firstNonEmpty(user.DisplayName, user.RealName, user.Name, user.ID)
}

var (
	slackMentionRe  = regexp.MustCompile(`<@([A-Z0-9]+)(?:\|[^>]+)?>`)
	slackLinkRe     = regexp.MustCompile(`<([^|>]+)\|([^>]+)>`)
	slackBareLinkRe = regexp.MustCompile(`<([^>]+)>`)
)

func cleanSlackTitleText(text string) string {
	text = slackMentionRe.ReplaceAllString(text, "@$1")
	text = slackLinkRe.ReplaceAllString(text, "$2")
	text = slackBareLinkRe.ReplaceAllString(text, "$1")
	replacer := strings.NewReplacer("\r", " ", "\n", " ", "\t", " ", "*", "", "`", "", "_", "")
	text = replacer.Replace(text)
	return truncateRunes(strings.Join(strings.Fields(text), " "), slackTitleContextLimit)
}

func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return strings.TrimSpace(string(runes[:max-1])) + "..."
}

func normalizeSlackChannelID(id string) string {
	return strings.ToUpper(strings.TrimSpace(id))
}

var resolveSlackTaskTitle = func(ctx context.Context, decision ReactionDecision) (string, error) {
	client := newSlackTitleAPIClient()
	if client == nil {
		return "", errors.New("slack token unavailable")
	}
	return BuildSlackTaskTitle(ctx, client, decision, SelfUserIDs())
}
