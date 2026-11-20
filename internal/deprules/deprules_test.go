// Package deprules enforces the bounded-context dependency rules
// (ADR-0005) with plain `go list` — no external linter, runs in the
// normal `go test ./...` gate.
package deprules

import (
	"os/exec"
	"strings"
	"testing"
)

// forbidden[from] = domains `from` may NOT import. Shared/store is a
// registered exception (ADR-0005 revision): the persistence adapter
// legitimately projects domain types, so `shared` here means
// shared/events + shared/acl + shared/rds + shared/migrate only.
var forbidden = map[string][]string{
	// Money flows one way: policy reads billing; billing never
	// reaches back (config reaches it via shared/events payloads).
	"github.com/gvos-io/gvos-core/internal/billingdomain": {
		"github.com/gvos-io/gvos-core/internal/customerdomain",
		"github.com/gvos-io/gvos-core/internal/mediamain",
	},
	// The event bus / acl / redis / migrate packages know no domains
	// (the store adapter is the sanctioned projection point).
	"github.com/gvos-io/gvos-core/internal/shared/events": {
		"github.com/gvos-io/gvos-core/internal/billingdomain",
		"github.com/gvos-io/gvos-core/internal/customerdomain",
		"github.com/gvos-io/gvos-core/internal/mediamain",
		"github.com/gvos-io/gvos-core/internal/callcore",
		"github.com/gvos-io/gvos-core/internal/api",
	},
	"github.com/gvos-io/gvos-core/internal/shared/rds": {
		"github.com/gvos-io/gvos-core/internal/billingdomain",
		"github.com/gvos-io/gvos-core/internal/customerdomain",
		"github.com/gvos-io/gvos-core/internal/mediamain",
		"github.com/gvos-io/gvos-core/internal/callcore",
		"github.com/gvos-io/gvos-core/internal/api",
	},
	"github.com/gvos-io/gvos-core/internal/shared/acl": {
		"github.com/gvos-io/gvos-core/internal/billingdomain",
		"github.com/gvos-io/gvos-core/internal/customerdomain",
		"github.com/gvos-io/gvos-core/internal/mediamain",
		"github.com/gvos-io/gvos-core/internal/callcore",
		"github.com/gvos-io/gvos-core/internal/api",
	},
	"github.com/gvos-io/gvos-core/internal/shared/migrate": {
		"github.com/gvos-io/gvos-core/internal/billingdomain",
		"github.com/gvos-io/gvos-core/internal/customerdomain",
		"github.com/gvos-io/gvos-core/internal/mediamain",
		"github.com/gvos-io/gvos-core/internal/callcore",
		"github.com/gvos-io/gvos-core/internal/api",
	},
	"github.com/gvos-io/gvos-core/internal/shared/recpipe": {
		"github.com/gvos-io/gvos-core/internal/api",
	},
}

func TestForbiddenDependencyEdges(t *testing.T) {
	for from, denied := range forbidden {
		// Directory namespaces need the /... pattern; cmd mains list
		// the package itself.
		pattern := from + "/..."
		if strings.HasSuffix(from, "cmd/gvos-core") {
			pattern = from
		}
		// Direct imports only (.Imports), never the transitive closure.
		cmd := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Imports \" \"}}", pattern)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pattern, err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			parts := strings.SplitN(strings.TrimSpace(line), " ", 2)
			if len(parts) != 2 {
				continue
			}
			importer := parts[0]
			for _, imp := range strings.Fields(parts[1]) {
				for _, d := range denied {
					if imp == d || (strings.HasPrefix(imp, d) && !strings.Contains(strings.TrimPrefix(imp, d), "/")) {
						t.Errorf("forbidden edge: %s imports %s (ADR-0005)", importer, imp)
					}
				}
			}
		}
	}
}
