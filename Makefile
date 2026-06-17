BINARY      := flow
INSTALL_DIR := $(HOME)/.local/bin

# VERSION is injected into the binary via -ldflags and shown in `flow
# --version` and Mission Control. `git describe` yields the exact tag on a
# released commit (e.g. v0.1.0-alpha.1) but a DEV stamp on anything past it —
# v0.1.0-alpha.1-<N>-g<sha>[-dirty] — so a local `make build`/`make rebuild` is
# always distinguishable from a release at a glance. A tagless checkout falls
# back to the short sha (--always), or "dev" with no git at all. The release
# workflow overrides this with the pushed tag (VERSION=<tag>).
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -X main.version=$(VERSION)

.PHONY: build ui ui-check rebuild install uninstall test clean

# The web UI bundles under internal/server/static/assets are NOT committed (only
# static/index.html + brand SVGs are), so on a fresh checkout they're absent.
# `//go:embed all:static` still compiles, but the UI won't render until built —
# so auto-build the UI when the bundle is missing. When it's present (normal dev
# after `make ui`), this is a no-op and `make build` stays a fast Node-free Go build.
build:
	@if [ -z "$$(ls internal/server/static/assets/index-*.js 2>/dev/null)" ]; then \
		echo "UI bundle missing — building it first..."; \
		$(MAKE) ui; \
	fi
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) .

# Rebuild the web UI (Vite + React + TypeScript) into internal/server/static.
# Run after editing UI source under internal/server/ui; the emitted bundles are
# gitignored, so commit only your source changes.
ui:
	cd internal/server/ui && pnpm install && pnpm run build

# Type-check the UI without emitting (fast feedback while editing UI source).
ui-check:
	cd internal/server/ui && pnpm install && pnpm run typecheck

# Rebuild the UI assets and then the Go binary that embeds them — the one-shot
# command after editing anything under internal/server/ui.
rebuild: ui build

test:
	go test ./...

install: build
	@# Place the binary in $(INSTALL_DIR) so the user's repo dir stays
	@# clean and `rm -rf` of this clone won't break their shell.
	@mkdir -p $(INSTALL_DIR)
	@cp $(BINARY) $(INSTALL_DIR)/$(BINARY)
	@echo "Installed $(BINARY) -> $(INSTALL_DIR)/$(BINARY)"
	@# Offer to add $(INSTALL_DIR) to PATH if it isn't already there.
	@case ":$$PATH:" in \
		*":$(INSTALL_DIR):"*) \
			echo "$(INSTALL_DIR) already in PATH." ;; \
		*) \
			rc_file="$$HOME/.zshrc"; \
			case "$$SHELL" in \
				*/bash) rc_file="$$HOME/.bashrc" ;; \
				*/fish) rc_file="$$HOME/.config/fish/config.fish" ;; \
			esac; \
			line='export PATH="$$HOME/.local/bin:$$PATH"'; \
			echo ""; \
			echo "$(INSTALL_DIR) is NOT in your PATH."; \
			echo "Suggested line for $$rc_file:"; \
			echo "  $$line"; \
			printf "Append it to %s now? [y/N] " "$$rc_file"; \
			read -r reply; \
			case "$$reply" in \
				[yY]|[yY][eE][sS]) \
					if grep -qF "$$line" "$$rc_file" 2>/dev/null; then \
						echo "$$rc_file already contains the line."; \
					else \
						echo "$$line" >> "$$rc_file"; \
						echo "Appended to $$rc_file. Run 'source $$rc_file' or open a new terminal."; \
					fi ;; \
				*) \
					echo "Skipped. Add the line to $$rc_file yourself, or invoke flow with the full path: $(INSTALL_DIR)/$(BINARY)" ;; \
			esac ;; \
	esac
	@# Install skill + SessionStart hook
	@./$(BINARY) skill install --force
	@echo ""
	@echo "Run 'flow init' to create ~/.flow/ and the database."

uninstall:
	@if [ -x "$(INSTALL_DIR)/$(BINARY)" ]; then \
		"$(INSTALL_DIR)/$(BINARY)" skill uninstall; \
		rm -f "$(INSTALL_DIR)/$(BINARY)"; \
		echo "Removed $(INSTALL_DIR)/$(BINARY)."; \
	elif [ -x "./$(BINARY)" ]; then \
		./$(BINARY) skill uninstall; \
	else \
		echo "No installed flow binary found. Skill/hook may already be removed."; \
	fi
	@echo "If you added $(INSTALL_DIR) to your shell rc file, remove that line manually."

clean:
	rm -f $(BINARY) flowde
