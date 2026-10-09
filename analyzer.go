package main

import (
	"fmt"
	"path"
	"sort"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// classifyFiles splits the files changed in a pull request into
// non-dependency files (immediate disqualifiers) and go.mod files to
// analyze. go.sum files are allowed but not parsed: version changes are
// read from go.mod. Paths are relative to the repository root, so
// multi-module repositories (Chainlink-style monorepos) are covered.
func classifyFiles(files []string) (offenders, goMods []string) {
	for _, f := range files {
		switch path.Base(f) {
		case "go.mod":
			goMods = append(goMods, f)
		case "go.sum":
			// Allowed: the dependency lock file.
		default:
			offenders = append(offenders, f)
		}
	}
	return offenders, goMods
}

// moduleChange is one changed go.mod file, parsed at the merge base and at
// the pull request head. A nil Base/Head means the file does not exist at
// that ref.
type moduleChange struct {
	Path             string
	Base, Head       *modfile.File
	BaseErr, HeadErr error
}

// decision is the outcome of evaluating every changed go.mod file.
type decision struct {
	Label    bool
	Reasons  []string
	Upgrades []string
	Notes    []string
}

func (d *decision) reject(format string, args ...any) {
	d.Reasons = append(d.Reasons, fmt.Sprintf(format, args...))
}

type replaceKey struct{ path, version string }
type replaceVal struct{ path, version string }

// evaluate returns the decision for the given go.mod changes. The label is
// applied only when every change is a pure dependency upgrade: at least one
// dependency version increases, nothing is downgraded, the Go version
// (go/toolchain directives) is untouched and there are no other structural
// changes.
func evaluate(changes []moduleChange) decision {
	var d decision
	for _, ch := range changes {
		switch {
		case ch.BaseErr != nil:
			d.reject("%s: base go.mod is unparseable: %v", ch.Path, ch.BaseErr)
			continue
		case ch.HeadErr != nil:
			d.reject("%s: head go.mod is unparseable: %v", ch.Path, ch.HeadErr)
			continue
		case ch.Base == nil:
			d.reject("%s: go.mod added", ch.Path)
			continue
		case ch.Head == nil:
			d.reject("%s: go.mod deleted", ch.Path)
			continue
		}

		if mp, hp := modulePath(ch.Base), modulePath(ch.Head); mp != hp {
			d.reject("%s: module path changed (%s -> %s)", ch.Path, mp, hp)
		}
		if bv, hv := goDirective(ch.Base), goDirective(ch.Head); bv != hv {
			d.reject("%s: Go version directive changed (%s -> %s): Go version upgrades and downgrades are not dependency bumps", ch.Path, bv, hv)
		}
		if bt, ht := toolchainDirective(ch.Base), toolchainDirective(ch.Head); bt != ht {
			d.reject("%s: toolchain directive changed (%s -> %s)", ch.Path, bt, ht)
		}

		compareRequires(ch.Path, requireMap(ch.Base), requireMap(ch.Head), &d)
		compareReplaces(ch.Path, replaceMap(ch.Base), replaceMap(ch.Head), &d)
		if !equalKeySets(excludeSet(ch.Base), excludeSet(ch.Head)) {
			d.reject("%s: exclude directives changed", ch.Path)
		}
		if !equalKeySets(retractSet(ch.Base), retractSet(ch.Head)) {
			d.reject("%s: retract directives changed", ch.Path)
		}
		if !equalKeySets(godebugSet(ch.Base), godebugSet(ch.Head)) {
			d.reject("%s: godebug directives changed", ch.Path)
		}
		if !equalKeySets(toolSet(ch.Base), toolSet(ch.Head)) {
			d.reject("%s: tool directives changed", ch.Path)
		}
		if !equalKeySets(ignoreSet(ch.Base), ignoreSet(ch.Head)) {
			d.reject("%s: ignore directives changed", ch.Path)
		}
	}
	if len(d.Reasons) == 0 && len(d.Upgrades) == 0 {
		d.reject("no dependency version upgrades found")
	}
	d.Label = len(d.Reasons) == 0
	return d
}

// compareVersion records an upgrade, a downgrade or an incomparable version
// change for name. Callers must only pass differing versions.
func compareVersion(d *decision, name, base, head string) {
	if !semver.IsValid(base) || !semver.IsValid(head) {
		d.reject("%s: version change is not semver-comparable (%s -> %s)", name, base, head)
		return
	}
	switch semver.Compare(head, base) {
	case 1:
		d.Upgrades = append(d.Upgrades, fmt.Sprintf("%s %s -> %s", name, base, head))
	case -1:
		d.reject("%s: dependency downgrade (%s -> %s)", name, base, head)
	}
}

func compareRequires(file string, base, head map[string]string, d *decision) {
	for _, name := range unionKeys(base, head) {
		bv, bok := base[name]
		hv, hok := head[name]
		switch {
		case bok && hok && bv != hv:
			compareVersion(d, file+": "+name, bv, hv)
		case bok && !hok:
			d.Notes = append(d.Notes, fmt.Sprintf("%s: dependency removed: %s %s", file, name, bv))
		case !bok && hok:
			d.Notes = append(d.Notes, fmt.Sprintf("%s: dependency added: %s %s", file, name, hv))
		}
	}
}

func compareReplaces(file string, base, head map[replaceKey]replaceVal, d *decision) {
	for _, k := range unionKeys(base, head) {
		bv, bok := base[k]
		hv, hok := head[k]
		switch {
		case bok && hok && bv != hv:
			if bv.path != hv.path {
				d.reject("%s: replace target for %s changed module path (%s -> %s)", file, k.path, bv.path, hv.path)
				continue
			}
			compareVersion(d, file+": replace "+k.path, bv.version, hv.version)
		case bok && !hok:
			d.Notes = append(d.Notes, fmt.Sprintf("%s: replace removed: %s => %s %s", file, k.path, bv.path, bv.version))
		case !bok && hok:
			d.Notes = append(d.Notes, fmt.Sprintf("%s: replace added: %s => %s %s", file, k.path, hv.path, hv.version))
		}
	}
}

func modulePath(f *modfile.File) string {
	if f.Module == nil {
		return ""
	}
	return f.Module.Mod.Path
}

func goDirective(f *modfile.File) string {
	if f.Go == nil {
		return ""
	}
	return f.Go.Version
}

func toolchainDirective(f *modfile.File) string {
	if f.Toolchain == nil {
		return ""
	}
	return f.Toolchain.Name
}

func requireMap(f *modfile.File) map[string]string {
	m := make(map[string]string)
	for _, r := range f.Require {
		m[r.Mod.Path] = r.Mod.Version
	}
	return m
}

func replaceMap(f *modfile.File) map[replaceKey]replaceVal {
	m := make(map[replaceKey]replaceVal)
	for _, r := range f.Replace {
		m[replaceKey{r.Old.Path, r.Old.Version}] = replaceVal{r.New.Path, r.New.Version}
	}
	return m
}

func excludeSet(f *modfile.File) map[string]bool {
	m := make(map[string]bool)
	for _, e := range f.Exclude {
		m[e.Mod.Path+"@"+e.Mod.Version] = true
	}
	return m
}

func retractSet(f *modfile.File) map[string]bool {
	m := make(map[string]bool)
	for _, r := range f.Retract {
		m[r.Low+".."+r.High] = true
	}
	return m
}

func godebugSet(f *modfile.File) map[string]bool {
	m := make(map[string]bool)
	for _, g := range f.Godebug {
		m[g.Key+"="+g.Value] = true
	}
	return m
}

func toolSet(f *modfile.File) map[string]bool {
	m := make(map[string]bool)
	for _, t := range f.Tool {
		m[t.Path] = true
	}
	return m
}

func ignoreSet(f *modfile.File) map[string]bool {
	m := make(map[string]bool)
	for _, i := range f.Ignore {
		m[i.Path] = true
	}
	return m
}

func equalKeySets(base, head map[string]bool) bool {
	if len(base) != len(head) {
		return false
	}
	for k := range base {
		if !head[k] {
			return false
		}
	}
	return true
}

// unionKeys returns the sorted union of the keys of the given maps.
func unionKeys[K comparable, V any](maps ...map[K]V) []K {
	set := make(map[K]struct{})
	for _, m := range maps {
		for k := range m {
			set[k] = struct{}{}
		}
	}
	keys := make([]K, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	return keys
}
