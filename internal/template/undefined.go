package template

import "errors"

// Undefined is Ansible's chainable strict-undefined: attribute and item
// access extend the name (so `foo.bar.baz is defined` works without
// erroring), while any other use — arithmetic, rendering, iteration,
// truthiness, or most filters — raises an UndefinedError. Only default/d,
// mandatory, and the defined/undefined tests accept it.
type Undefined struct {
	Name string // dotted path for the message: "foo.bar"

	// Err, when set, is an error captured instead of raised (a failed
	// call assigned by {% set %}, ansible-core's captured-exception
	// marker): using the value raises it.
	Err error
}

// useError is the error using an undefined value raises.
func (u Undefined) useError(pos Position) error {
	if u.Err != nil {
		return u.Err
	}
	return &UndefinedError{Pos: pos, Name: u.Name}
}

// captureSetError is how {% set %} treats its value's error: a call or
// subscript that raised is bound as a marker that raises when used (so
// `{% set _ = x.pop() %}` never used cannot fail the template); other
// errors raise now.
func captureSetError(e Expr, err error) (any, error) {
	switch e.(type) {
	case *callExpr, *getItemExpr:
		var te *TemplateError
		var ue *UndefinedError
		if (errors.As(err, &te) && !te.Syntax) || errors.As(err, &ue) {
			return Undefined{Name: "captured error", Err: err}, nil
		}
	}
	return nil, err
}

// Omit is the value of the `omit` magic variable. A module argument whose
// final templated value is Omit is dropped by the executor.
type Omit struct{}

// UndefinedError is the strict-undefined use error.
type UndefinedError struct {
	Pos  Position
	Name string
	// Hint, when set, is the undefined value's own message ("No first
	// item, sequence was empty.").
	Hint string
}

func (e *UndefinedError) Error() string {
	return sprintf("%s: %q is undefined", e.Pos, e.Name)
}

func isUndefined(v any) bool {
	_, ok := v.(Undefined)
	return ok
}
