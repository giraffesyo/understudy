package dnspy

import (
	"fmt"
	"strings"
)

// Error is a dnspython exception: its class and str(). DNSException
// subclasses can be told from plain Python exceptions (ValueError, ...),
// which the dig lookup does not catch.
type Error struct {
	Class string // the exception class (NXDOMAIN, NoAnswer, ValueError, ...)
	Msg   string
	// Python marks a plain Python exception, not a DNSException.
	Python bool
	// Syntax and Form mark dns.exception.SyntaxError and FormError
	// subclasses.
	Syntax, Form bool
	// OS marks an OSError (the nameserver is dropped for it).
	OS bool
	// EOF marks an EOFError.
	EOF bool
	// Timeout marks dns.exception.Timeout and its LifetimeTimeout.
	Timeout bool
}

func (e *Error) Error() string { return e.Msg }

// Is reports whether err is the dnspython exception class.
func Is(err error, class string) bool {
	e, ok := err.(*Error)
	return ok && e.Class == class
}

// IsDNSException reports whether err is a dns.exception.DNSException.
func IsDNSException(err error) bool {
	e, ok := err.(*Error)
	return ok && !e.Python && !e.OS && !e.EOF
}

var (
	errFormError   = &Error{Class: "FormError", Msg: "DNS message is malformed.", Form: true}
	errSyntax      = &Error{Class: "SyntaxError", Msg: "Text input is malformed.", Syntax: true}
	errTimeout     = &Error{Class: "Timeout", Msg: "The DNS operation timed out.", Timeout: true}
	errTruncated   = &Error{Class: "Truncated", Msg: "The truncated flag is set."}
	errChainTooLng = &Error{Class: "ChainTooLong", Msg: "The CNAME chain is too long."}
	errAnswerForNX = &Error{Class: "AnswerForNXDOMAIN", Msg: "The rcode is NXDOMAIN but an answer was found."}
	errYXDOMAIN    = &Error{Class: "YXDOMAIN", Msg: "The DNS query name is too long after DNAME substitution."}
	errMetaqueries = &Error{Class: "NoMetaqueries", Msg: "DNS metaqueries are not allowed."}
	errUnknownType = &Error{Class: "UnknownRdatatype", Msg: "DNS resource record type is unknown."}
	errUnknownCls  = &Error{Class: "UnknownRdataclass", Msg: "A DNS class is unknown."}
)

func formError(msg string) *Error {
	return &Error{Class: "FormError", Msg: msg, Form: true}
}

func valueError(format string, args ...any) *Error {
	return &Error{Class: "ValueError", Msg: fmt.Sprintf(format, args...), Python: true}
}

// serverError is one entry of a resolution's errors: the nameserver and
// what it answered.
type serverError struct {
	server string
	what   string
}

func errorsText(errs []serverError) string {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = "Server " + e.server + " answered " + e.what
	}
	return strings.Join(parts, "; ")
}

// nxdomain is NXDOMAIN(qnames=...).
func nxdomain(qnames []Name) *Error {
	msg := "The DNS query name does not exist"
	if len(qnames) > 1 {
		msg = "None of DNS query names exist"
	}
	parts := make([]string, len(qnames))
	for i, q := range qnames {
		parts[i] = q.String()
	}
	return &Error{Class: "NXDOMAIN", Msg: msg + ": " + strings.Join(parts, ", ")}
}

// question is a message's question as NoAnswer and NoNameservers show
// it: "name. IN A".
func questionText(q question) string {
	return q.name.String() + " " + ClassText(q.class) + " " + TypeText(q.typ)
}

func noAnswer(q question) *Error {
	return &Error{Class: "NoAnswer", Msg: "The DNS response does not contain an answer to the question: " + questionText(q)}
}

func noNameservers(q question, errs []serverError) *Error {
	return &Error{Class: "NoNameservers", Msg: "All nameservers failed to answer the query " + questionText(q) + ": " + errorsText(errs)}
}

func lifetimeTimeout(seconds float64, errs []serverError) *Error {
	return &Error{Class: "LifetimeTimeout", Msg: fmt.Sprintf("The resolution lifetime expired after %.3f seconds: %s", seconds, errorsText(errs)), Timeout: true}
}
