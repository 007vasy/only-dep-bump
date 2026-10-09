package main

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

func mustParse(t *testing.T, src string) *modfile.File {
	t.Helper()
	f, err := modfile.Parse("go.mod", []byte(src), nil)
	if err != nil {
		t.Fatalf("parse go.mod: %v", err)
	}
	return f
}

func changeOf(t *testing.T, base, head string) moduleChange {
	return moduleChange{Path: "go.mod", Base: mustParse(t, base), Head: mustParse(t, head)}
}

func wantDecision(t *testing.T, d decision, label bool, reasonParts ...string) {
	t.Helper()
	if d.Label != label {
		t.Fatalf("label = %t, want %t (reasons: %v)", d.Label, label, d.Reasons)
	}
	for _, part := range reasonParts {
		found := false
		for _, r := range d.Reasons {
			if strings.Contains(r, part) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no reason contains %q, reasons: %v", part, d.Reasons)
		}
	}
}

const baseGoMod = `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/other/dep v1.4.2
	golang.org/x/mod v0.39.0
)
`

func TestClassifyFiles(t *testing.T) {
	offenders, goMods := classifyFiles([]string{
		"go.mod", "go.sum", "nested/go.mod", "nested/go.sum",
		"main.go", "README.md", ".tool-versions", ".github/workflows/ci.yml",
	})
	if got := strings.Join(offenders, ","); got != "main.go,README.md,.tool-versions,.github/workflows/ci.yml" {
		t.Errorf("offenders = %q", got)
	}
	if got := strings.Join(goMods, ","); got != "go.mod,nested/go.mod" {
		t.Errorf("goMods = %q", got)
	}
}

func TestEvaluatePureUpgrade(t *testing.T) {
	head := `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/other/dep v1.4.3
	golang.org/x/mod v0.40.0
)
`
	d := evaluate([]moduleChange{changeOf(t, baseGoMod, head)})
	wantDecision(t, d, true)
	if len(d.Upgrades) != 2 {
		t.Fatalf("upgrades = %v, want 2", d.Upgrades)
	}
}

func TestEvaluateDowngrade(t *testing.T) {
	head := `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/other/dep v1.4.1
	golang.org/x/mod v0.39.0
)
`
	d := evaluate([]moduleChange{changeOf(t, baseGoMod, head)})
	wantDecision(t, d, false, "dependency downgrade", "github.com/other/dep: dependency downgrade (v1.4.2 -> v1.4.1)")
}

func TestEvaluateGoDirectiveChange(t *testing.T) {
	downgrade := `module github.com/example/repo/v2

go 1.27.0

require (
	github.com/other/dep v1.4.2
	golang.org/x/mod v0.39.0
)
`
	upgrade := `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/other/dep v1.4.2
	golang.org/x/mod v0.40.0
)

toolchain go1.27.2
`
	wantDecision(t, evaluate([]moduleChange{changeOf(t, baseGoMod, downgrade)}), false, "Go version directive changed (1.27.1 -> 1.27.0)")
	// Both a toolchain change and an upgrade present: the toolchain change wins.
	wantDecision(t, evaluate([]moduleChange{changeOf(t, baseGoMod, upgrade)}), false, "toolchain directive changed")
}

func TestEvaluateToolchainDirectiveChange(t *testing.T) {
	head := `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/other/dep v1.4.2
	golang.org/x/mod v0.39.0
)

toolchain go1.27.2
`
	d := evaluate([]moduleChange{changeOf(t, baseGoMod, head)})
	wantDecision(t, d, false, "toolchain directive changed")
}

func TestEvaluateNoUpgrades(t *testing.T) {
	additionOnly := `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/new/dep v1.0.0
	github.com/other/dep v1.4.2
	golang.org/x/mod v0.39.0
)
`
	removalOnly := `module github.com/example/repo/v2

go 1.27.1

require github.com/other/dep v1.4.2
`
	sameVersionsIndirect := `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/other/dep v1.4.2
	golang.org/x/mod v0.39.0 // indirect
)
`
	for name, head := range map[string]string{
		"addition only": additionOnly,
		"removal only":  removalOnly,
		"indirect move": sameVersionsIndirect,
	} {
		t.Run(name, func(t *testing.T) {
			d := evaluate([]moduleChange{changeOf(t, baseGoMod, head)})
			wantDecision(t, d, false, "no dependency version upgrades found")
		})
	}
}

func TestEvaluateGoSumOnly(t *testing.T) {
	d := evaluate(nil)
	wantDecision(t, d, false, "no dependency version upgrades found")
}

func TestEvaluateReplaceUpgrade(t *testing.T) {
	base := `module github.com/example/repo

go 1.27.1

require golang.org/x/mod v0.39.0

replace golang.org/x/mod => github.com/example/fork v0.39.1
`
	head := `module github.com/example/repo

go 1.27.1

require golang.org/x/mod v0.39.0

replace golang.org/x/mod => github.com/example/fork v0.39.2
`
	d := evaluate([]moduleChange{changeOf(t, base, head)})
	wantDecision(t, d, true)
	if len(d.Upgrades) != 1 || !strings.Contains(d.Upgrades[0], "v0.39.1 -> v0.39.2") {
		t.Fatalf("upgrades = %v", d.Upgrades)
	}
}

func TestEvaluateReplaceTargetPathChange(t *testing.T) {
	base := `module github.com/example/repo

go 1.27.1

require github.com/example/dep v1.0.0

replace github.com/example/dep => ../local
`
	head := `module github.com/example/repo

go 1.27.1

require github.com/example/dep v1.0.0

replace github.com/example/dep => github.com/example/dep-fork v1.0.0
`
	d := evaluate([]moduleChange{changeOf(t, base, head)})
	wantDecision(t, d, false, "replace target for github.com/example/dep changed module path")
}

func TestEvaluateReplaceDowngrade(t *testing.T) {
	base := `module github.com/example/repo

go 1.27.1

require golang.org/x/mod v0.39.0

replace golang.org/x/mod => github.com/example/fork v0.39.2
`
	head := `module github.com/example/repo

go 1.27.1

require golang.org/x/mod v0.39.0

replace golang.org/x/mod => github.com/example/fork v0.39.1
`
	d := evaluate([]moduleChange{changeOf(t, base, head)})
	wantDecision(t, d, false, "dependency downgrade")
}

func TestEvaluateStructuralGoModChanges(t *testing.T) {
	moduleRenamed := `module github.com/example/renamed/v2

go 1.27.1

require (
	github.com/other/dep v1.4.2
	golang.org/x/mod v0.39.0
)
`
	excludeAdded := `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/other/dep v1.4.2
	golang.org/x/mod v0.39.0
)

exclude github.com/other/dep v1.4.2
`
	retractAdded := `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/other/dep v1.4.2
	golang.org/x/mod v0.39.0
)

retract [v1.4.0, v1.4.2]
`
	godebugAdded := `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/other/dep v1.4.2
	golang.org/x/mod v0.39.0
)

godebug default=go1.x
`
	toolAdded := `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/other/dep v1.4.2
	golang.org/x/mod v0.39.0
)

tool golang.org/x/tools/cmd/goimports
`
	for name, tc := range map[string]struct {
		head   string
		reason string
	}{
		"module renamed": {moduleRenamed, "module path changed"},
		"exclude added":  {excludeAdded, "exclude directives changed"},
		"retract added":  {retractAdded, "retract directives changed"},
		"godebug added":  {godebugAdded, "godebug directives changed"},
		"tool added":     {toolAdded, "tool directives changed"},
	} {
		t.Run(name, func(t *testing.T) {
			d := evaluate([]moduleChange{changeOf(t, baseGoMod, tc.head)})
			wantDecision(t, d, false, tc.reason)
		})
	}
}

func TestEvaluateModuleFilesAddedDeletedUnparseable(t *testing.T) {
	added := evaluate([]moduleChange{{Path: "new/go.mod", Head: mustParse(t, baseGoMod)}})
	wantDecision(t, added, false, "new/go.mod: go.mod added")

	deleted := evaluate([]moduleChange{{Path: "go.mod", Base: mustParse(t, baseGoMod)}})
	wantDecision(t, deleted, false, "go.mod: go.mod deleted")

	unparseable := evaluate([]moduleChange{{
		Path:    "go.mod",
		Base:    mustParse(t, baseGoMod),
		HeadErr: errors.New("bad directive"),
	}})
	wantDecision(t, unparseable, false, "head go.mod is unparseable")
}

func TestEvaluateMultiModuleMixed(t *testing.T) {
	root := changeOf(t, baseGoMod, `module github.com/example/repo/v2

go 1.27.1

require (
	github.com/other/dep v1.4.3
	golang.org/x/mod v0.39.0
)
`)
	nested := moduleChange{Path: "nested/go.mod"}
	nested.Base = mustParse(t, `module github.com/example/repo/nested

go 1.27.1

require github.com/other/dep v1.4.2
`)
	nested.Head = mustParse(t, `module github.com/example/repo/nested

go 1.27.1

require github.com/other/dep v1.4.1
`)
	d := evaluate([]moduleChange{root, nested})
	wantDecision(t, d, false, "nested/go.mod: github.com/other/dep: dependency downgrade")
}

func TestEvaluatePseudoVersionUpgrade(t *testing.T) {
	// Chainlink-style internal module bumps use pseudo-versions.
	base := `module github.com/example/repo

go 1.27.1

require github.com/smartcontractkit/chainlink-common v0.11.2-0.20261007174852-7fabc093ff84
`
	head := `module github.com/example/repo

go 1.27.1

require github.com/smartcontractkit/chainlink-common v0.11.2-0.20261008155618-50f521d70e62
`
	d := evaluate([]moduleChange{changeOf(t, base, head)})
	wantDecision(t, d, true)
	if len(d.Upgrades) != 1 {
		t.Fatalf("upgrades = %v, want 1", d.Upgrades)
	}

	pseudoDowngrade := `module github.com/example/repo

go 1.27.1

require github.com/smartcontractkit/chainlink-common v0.11.1
`
	d = evaluate([]moduleChange{changeOf(t, base, pseudoDowngrade)})
	wantDecision(t, d, false, "dependency downgrade")
}
