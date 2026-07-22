package template

// Undefined is Ansible's chainable strict-undefined: attribute and item
// access extend the name (so `foo.bar.baz is defined` works without
// erroring), while any other use — arithmetic, rendering, iteration,
// truthiness, or most filters — raises an UndefinedError. Only default/d,
// mandatory, and the defined/undefined tests accept it.
type Undefined struct {
	Name string // dotted path for the message: "foo.bar"
}

// Omit is the value of the `omit` magic variable. A module argument whose
// final templated value is Omit is dropped by the executor.
type Omit struct{}

// UndefinedError is the strict-undefined use error.
type UndefinedError struct {
	Pos  Position
	Name string
}

func (e *UndefinedError) Error() string {
	return sprintf("%s: %q is undefined", e.Pos, e.Name)
}

func isUndefined(v any) bool {
	_, ok := v.(Undefined)
	return ok
}
