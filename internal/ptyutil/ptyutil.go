// Package ptyutil opens pseudo-terminals with the standard library only. It
// is shared by the controller (su/doas become on a local connection) and
// the agent (modules that answer prompts on a terminal), so it must stay
// free of control-plane imports.
package ptyutil
