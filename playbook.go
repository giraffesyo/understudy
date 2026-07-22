// Package understudy lets Go programs define Ansible-compatible playbooks
// in native Go, then either render them to standard playbook YAML or run
// them directly through the embedded execution engine.
//
//	pb := understudy.Playbook{{
//		Name:  "Configure web servers",
//		Hosts: "web",
//		Tasks: []understudy.Task{
//			{Name: "install nginx", Action: understudy.Package{Name: "nginx"}},
//			{Name: "render config", Action: understudy.Template{
//				Src: "nginx.conf.j2", Dest: "/etc/nginx/nginx.conf", Mode: "0644",
//			}, Notify: []string{"restart nginx"}},
//		},
//		Handlers: []understudy.Task{
//			{Name: "restart nginx", Action: understudy.Service{Name: "nginx", State: "restarted"}},
//		},
//	}}
//
//	yamlBytes, _ := pb.YAML()                  // valid ansible-playbook input
//	result, _ := understudy.Run(ctx, pb, opts) // or run it directly
package understudy

// Playbook is an ordered list of plays.
type Playbook []Play

// Play targets a host pattern with tasks, mirroring an Ansible play.
type Play struct {
	Name        string
	Hosts       string // host pattern; required
	Vars        map[string]any
	VarsFiles   []string
	GatherFacts *bool // nil = gather (Ansible default)
	Become      bool
	BecomeUser  string
	Tags        []string
	Environment map[string]any
	PreTasks    []Task
	Tasks       []Task
	PostTasks   []Task
	Handlers    []Task
}

// Task is one unit of work. Action is required; everything else mirrors
// Ansible's task keywords.
type Task struct {
	Name         string
	Action       Action
	When         []string // Jinja expressions, ANDed
	Loop         any      // list, or a "{{ ... }}" template string
	LoopVar      string   // default "item"
	Register     string
	IgnoreErrors bool
	FailedWhen   []string
	ChangedWhen  []string
	Until        string
	Retries      int
	Delay        int
	Become       *bool // nil = inherit from play
	BecomeUser   string
	Vars         map[string]any
	Environment  map[string]any
	Notify       []string
	Tags         []string
	NoLog        bool
	DelegateTo   string

	// Block groups nested tasks with rescue/always semantics. When set,
	// Action must be nil and the task-level keywords above apply to the
	// whole block (Ansible's block keyword inheritance).
	Block  []Task
	Rescue []Task
	Always []Task
}

// Action is what a task executes: a typed module struct (Copy, Service,
// ...) or the generic M for anything else.
type Action interface {
	// ModuleName returns the Ansible module name ("copy", "service").
	ModuleName() string
	// ModuleArgs returns the module arguments, omitting unset fields.
	ModuleArgs() map[string]any
}

// freeFormer is implemented by actions whose primary input is a raw string
// (command, shell, raw, script) rather than named arguments.
type freeFormer interface {
	freeForm() string
}

// M is the generic action: any module by name with raw arguments. Use it
// for modules without a typed wrapper.
//
//	understudy.M{Module: "community.general.ufw", Args: understudy.Args{"rule": "allow", "port": "22"}}
type M struct {
	Module string
	Args   map[string]any
	// Cmd is the free-form parameter for command/shell/raw-style modules.
	Cmd string
}

// Args is a convenience alias for module argument maps.
type Args = map[string]any

func (m M) ModuleName() string { return m.Module }

func (m M) ModuleArgs() map[string]any { return m.Args }

func (m M) freeForm() string { return m.Cmd }
