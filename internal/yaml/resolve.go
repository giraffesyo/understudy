package yaml

// This file ports PyYAML's implicit resolver rules (YAML 1.1), which is what
// Ansible uses (implicitResolvers, shared with the dumper). The famous
// gotchas are deliberately replicated:
//   - yes/no/on/off (word list only; single 'y'/'n' are NOT booleans)
//   - 0644 is a legacy octal int (=420); 0o17 is a string
//   - 1.10 is the float 1.1
//   - 1e5 is a STRING (PyYAML's float regex requires a sign after e/E)
//   - 1:20 is the sexagesimal int 80, 1:20.5 the float 80.5
//   - 2001-12-14 is a datetime.date, 2001-12-14 21:59:43 a datetime

// resolveScalar applies YAML 1.1 implicit typing to a plain scalar and
// constructs its value, as PyYAML's constructor does (a timestamp that
// is no valid date fails with datetime's ValueError).
func resolveScalar(s string) (any, error) {
	switch resolveTag(s, true) {
	case tagNull:
		return nil, nil
	case tagBool:
		return boolWords[s], nil
	case tagInt:
		return constructInt(s)
	case tagFloat:
		return constructFloat(s)
	case tagTime:
		return constructTimestamp(s)
	}
	return s, nil
}

var boolWords = map[string]bool{
	"yes": true, "Yes": true, "YES": true,
	"true": true, "True": true, "TRUE": true,
	"on": true, "On": true, "ON": true,
	"no": false, "No": false, "NO": false,
	"false": false, "False": false, "FALSE": false,
	"off": false, "Off": false, "OFF": false,
}
