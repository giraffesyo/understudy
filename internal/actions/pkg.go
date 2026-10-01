package actions

import (
	"context"
	"fmt"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules"
	"github.com/giraffesyo/understudy/internal/template"
)

func init() {
	Register("package", actionFunc(runPackageAction))
	Register("dnf", actionFunc(runDnfAction))
	Register("yum", actionFunc(runDnfAction))
}

// Keys shared with the package modules (internal/modules/pkg.go).
const (
	pkgMgrFactKey    = "_understudy_pkg_mgr"
	pkgUseVarKey     = "_understudy_package_use"
	pkgReportFactKey = "_understudy_report_pkg_mgr"
)

// hostPkgMgr is the host's ansible_facts.pkg_mgr, "" when unknown.
func hostPkgMgr(actx *Context) string {
	v, ok := actx.Vars.Get("ansible_facts")
	if !ok {
		return ""
	}
	var pm any
	switch f := v.(type) {
	case map[string]any:
		pm = f["pkg_mgr"]
	case template.Mapping:
		pm, _ = f.GetItem("pkg_mgr")
	}
	if s, ok := pm.(string); ok {
		return s
	}
	return ""
}

func runPkgModule(ctx context.Context, actx *Context, module string, args map[string]any) *agentproto.Result {
	req := &agentproto.TaskRequest{
		Proto:        agentproto.ProtoVersion,
		Op:           "task",
		Module:       module,
		Args:         args,
		CheckMode:    actx.CheckMode,
		Diff:         actx.Diff,
		Background:   actx.Background,
		AsyncTimeout: actx.AsyncTimeout,
	}
	res, err := actx.RunModule(ctx, req, nil)
	if err != nil {
		return agentproto.Fail("module execution failed: %v", err)
	}
	if _, ok := res.AnsibleFacts["discovered_interpreter_python"]; ok {
		// The action's pkg_mgr fact gives way to the module result's
		// facts (result.update(module result)).
		delete(res.AnsibleFacts, "pkg_mgr")
	}
	return res
}

// runPackageAction is the package action plugin's backend choice inputs:
// the ansible_package_use variable and the pkg_mgr fact.
func runPackageAction(ctx context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	fwd := map[string]any{}
	for k, v := range args {
		fwd[k] = v
	}
	if pm := hostPkgMgr(actx); pm != "" {
		fwd[pkgMgrFactKey] = pm
	}
	use := "auto"
	if v, ok := args["use"]; ok {
		use = fmt.Sprint(v)
	}
	if v, ok := actx.Vars.Get("ansible_package_use"); ok && v != nil && use == "auto" {
		if tv, err := actx.Vars.TemplateValue(v); err == nil {
			v = tv
		}
		if s := fmt.Sprint(v); s != "" {
			fwd[pkgUseVarKey] = s
			use = s
		}
	}
	// module_loader.has_plugin: a named module understudy does not have.
	if use != "auto" && use != "" && !modules.Exists(use) {
		return actionRaise("Could not find a matching action for the \"%s\" package manager.", use)
	}
	return runPkgModule(ctx, actx, "package", fwd)
}

// runDnfAction is the dnf action plugin (yum redirects to it): the
// pkg_mgr fact picks the backend; when that names no dnf backend the
// module detects it and reports it as a fact.
func runDnfAction(ctx context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	_, hasUse := args["use"]
	if _, hasBackend := args["use_backend"]; hasUse && hasBackend {
		return actionRaise("parameters are mutually exclusive: ('use', 'use_backend')")
	}
	fwd := map[string]any{}
	for k, v := range args {
		fwd[k] = v
	}
	if pm := hostPkgMgr(actx); pm != "" {
		fwd[pkgMgrFactKey] = pm
	}
	// A backend setup had to detect is reported as a fact, unless the
	// task is delegated without delegate_facts.
	if !actx.Delegated || actx.DelegateFacts {
		fwd[pkgReportFactKey] = true
	}
	return runPkgModule(ctx, actx, "dnf", fwd)
}
