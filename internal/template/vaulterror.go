package template

import "errors"

// VaultError is the use of an encrypted value that cannot be decrypted:
// ansible-core's "Attempt to use undecryptable variable: ...", raised at
// the value's origin.
type VaultError struct {
	Reason string // "Attempting to decrypt but no vault secrets found."
	Pos    Position
}

func (e *VaultError) Error() string { return "Attempt to use undecryptable variable: " + e.Reason }

// Vault errors' reasons.
const (
	VaultNoSecrets  = "Attempting to decrypt but no vault secrets found."
	VaultCannotOpen = "Decryption failed (no vault secrets were found that could decrypt)."
)

// AsVaultError finds a VaultError in err's chain.
func AsVaultError(err error) (*VaultError, bool) {
	var ve *VaultError
	ok := errors.As(err, &ve)
	return ve, ok
}

// UndefinedElsewhere reports an undefined value used by a template other
// than the one at pos (a variable's own template, an item of a
// variable's container): ansible-core raises it where that template is.
func UndefinedElsewhere(err error, pos Position) (Position, bool) {
	var ue *UndefinedError
	if !errors.As(err, &ue) || ue.Pos.File == "" || ue.Pos.Line == 0 || ue.Pos.WholeFile {
		return Position{}, false
	}
	if ue.Pos.File == pos.File && ue.Pos.Line == pos.Line && ue.Pos.Col == pos.Col {
		return Position{}, false
	}
	return Position{File: ue.Pos.File, Line: ue.Pos.Line, Col: ue.Pos.Col}, true
}

// RenderingCause is a template error ansible-core shows as the rendering
// error caused by another event (a value variable storage does not
// support, an undecryptable variable): that event's message, its origin
// (Pos.File empty: none, then value is its source) and value. ok is false
// for other errors.
func RenderingCause(err error) (msg string, pos Position, value string, ok bool) {
	if ve, isVault := AsVaultError(err); isVault {
		return ve.Error(), ve.Pos, "", true
	}
	if se, isStorage := AsStorageError(err); isStorage {
		return se.Error(), Position{}, se.Value, true
	}
	return "", Position{}, "", false
}
