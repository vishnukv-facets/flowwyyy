package flowclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type Version struct {
	Version string
	Schema  int
}

// requiredSchemaFloor is the minimum flow.db schema version flowwyyy requires of
// the (official) `flow` binary it execs. It is flowwyyy's OWN requirement in the
// two-binary model, so it lives product-side rather than reaching into the core
// flowdb package. Bump it when flowwyyy starts depending on a newer core schema.
// (Mirrors the value of flowdb.SchemaVersion at the time of decoupling.)
const requiredSchemaFloor = 1

var RequiredFloor = Version{Version: "dev", Schema: requiredSchemaFloor}

func CheckCompat(ctx context.Context, bin string, floor Version) error {
	stdout, stderr, code, err := (Client{Bin: bin}).Run(ctx, "version", "--json")
	if err != nil {
		return fmt.Errorf("run flow version --json: exit %d: %s", code, strings.TrimSpace(stderr))
	}
	var got struct {
		Version string `json:"version"`
		Schema  int    `json:"schema"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		return fmt.Errorf("parse flow version --json: %w", err)
	}
	if compareVersions(got.Version, floor.Version) < 0 || got.Schema < floor.Schema {
		return fmt.Errorf("flow binary %s schema %d is below required %s schema %d; upgrade flow or set $FLOW_BIN to a compatible binary",
			got.Version, got.Schema, floor.Version, floor.Schema)
	}
	return nil
}

func compareVersions(a, b string) int {
	if b == "" || b == "dev" || a == "dev" {
		return 0
	}
	ap := versionParts(a)
	bp := versionParts(b)
	for i := 0; i < len(ap) || i < len(bp); i++ {
		av, bv := 0, 0
		if i < len(ap) {
			av = ap[i]
		}
		if i < len(bp) {
			bv = bp[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if before, _, ok := strings.Cut(v, "-"); ok {
		v = before
	}
	fields := strings.Split(v, ".")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, _ := strconv.Atoi(f)
		out = append(out, n)
	}
	return out
}
