package mysqlclient

import (
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Column types (FIELD_TYPE).
const (
	TypeDecimal    = 0
	TypeTiny       = 1
	TypeShort      = 2
	TypeLong       = 3
	TypeFloat      = 4
	TypeDouble     = 5
	TypeNull       = 6
	TypeTimestamp  = 7
	TypeLongLong   = 8
	TypeInt24      = 9
	TypeDate       = 10
	TypeTime       = 11
	TypeDatetime   = 12
	TypeYear       = 13
	TypeVarchar    = 15
	TypeBit        = 16
	TypeJSON       = 245
	TypeNewDecimal = 246
	TypeEnum       = 247
	TypeSet        = 248
	TypeTinyBlob   = 249
	TypeMediumBlob = 250
	TypeLongBlob   = 251
	TypeBlob       = 252
	TypeVarString  = 253
	TypeString     = 254
	TypeGeometry   = 255
)

const binaryCharset = 63

// Field describes one result column.
type Field struct {
	Name, Table string
	Type        byte
	Flags       uint16
	Charset     uint16
}

// Result is one statement's outcome: a row set, or an OK packet's counts.
type Result struct {
	Fields       []Field
	Rows         [][][]byte // nil cell = NULL
	AffectedRows uint64
	InsertID     uint64
	Warnings     uint16
	Info         string
}

// RowCount is DB-API cursor.rowcount: the number of rows of a result set,
// else the affected-row count.
func (r *Result) RowCount() int64 {
	if r.Fields != nil {
		return int64(len(r.Rows))
	}
	return int64(r.AffectedRows)
}

// NoBackslashEscapes reports the session's NO_BACKSLASH_ESCAPES mode (as
// of the last server status), which changes string literal escaping.
func (c *Conn) NoBackslashEscapes() bool { return c.status&statusNoBackslashEscapes != 0 }

// InTransaction reports whether a transaction is open.
func (c *Conn) InTransaction() bool { return c.status&statusInTrans != 0 }

// Exec interpolates args into query (see Mogrify) and runs it.
func (c *Conn) Exec(query string, args any) (*Result, string, error) {
	if args != nil {
		q, err := Mogrify(query, args, c.NoBackslashEscapes())
		if err != nil {
			return nil, query, err
		}
		query = q
	}
	res, err := c.Query(query)
	return res, query, err
}

// Query runs one statement over the text protocol and returns its first
// result; further results (MULTI_RESULTS, e.g. from CALL) are drained.
func (c *Conn) Query(sql string) (*Result, error) {
	if c.nc == nil {
		return nil, &Error{Code: 0, Msg: ""}
	}
	c.seq = 0
	if err := c.writePacket(append([]byte{0x03}, sql...)); err != nil {
		return nil, err
	}
	res, err := c.readResult()
	if err != nil {
		return nil, err
	}
	for c.status&statusMoreResultsExists != 0 {
		if _, err := c.readResult(); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// Commit and Rollback end the current transaction.
func (c *Conn) Commit() error {
	_, err := c.Query("COMMIT")
	return err
}

func (c *Conn) Rollback() error {
	_, err := c.Query("ROLLBACK")
	return err
}

func (c *Conn) readOK(data []byte) *Result {
	res := &Result{}
	i := 1
	n, i, _, err := readLenenc(data, i)
	if err != nil {
		return res
	}
	res.AffectedRows = n
	n, i, _, err = readLenenc(data, i)
	if err != nil {
		return res
	}
	res.InsertID = n
	if i+4 <= len(data) {
		c.status = binary.LittleEndian.Uint16(data[i:])
		res.Warnings = binary.LittleEndian.Uint16(data[i+2:])
		res.Info = string(data[i+4:])
	}
	return res
}

func isEOF(data []byte) bool { return len(data) > 0 && data[0] == 0xfe && len(data) < 9 }

func (c *Conn) readEOF(data []byte) uint16 {
	var warnings uint16
	if len(data) >= 5 {
		warnings = binary.LittleEndian.Uint16(data[1:])
		c.status = binary.LittleEndian.Uint16(data[3:])
	}
	return warnings
}

func (c *Conn) readResult() (*Result, error) {
	data, err := c.readPacket()
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, malformed()
	}
	switch data[0] {
	case 0x00:
		return c.readOK(data), nil
	case 0xff:
		c.status &^= statusMoreResultsExists
		return nil, parseErrPacket(data)
	case 0xfb:
		// LOAD DATA LOCAL INFILE: refused, as with local_infile=False.
		if err := c.writePacket(nil); err != nil {
			return nil, err
		}
		if _, err := c.readPacket(); err != nil {
			return nil, err
		}
		return nil, &Error{Code: -1, Msg: "**WARN**: Received LOAD_LOCAL packet but local_infile option is false.", Single: true}
	}
	count, _, _, err := readLenenc(data, 0)
	if err != nil {
		return nil, err
	}
	res := &Result{Fields: make([]Field, 0, count)}
	for k := uint64(0); k < count; k++ {
		data, err := c.readPacket()
		if err != nil {
			return nil, err
		}
		f, err := parseField(data)
		if err != nil {
			return nil, err
		}
		res.Fields = append(res.Fields, f)
	}
	data, err = c.readPacket()
	if err != nil {
		return nil, err
	}
	if !isEOF(data) {
		return nil, malformed()
	}
	c.readEOF(data)
	res.Rows = [][][]byte{}
	for {
		data, err := c.readPacket()
		if err != nil {
			return nil, err
		}
		if isEOF(data) {
			res.Warnings = c.readEOF(data)
			return res, nil
		}
		if len(data) > 0 && data[0] == 0xff {
			c.status &^= statusMoreResultsExists
			return nil, parseErrPacket(data)
		}
		row := make([][]byte, count)
		i := 0
		for k := range row {
			v, next, null, err := readLenencBytes(data, i)
			if err != nil {
				return nil, err
			}
			i = next
			if !null {
				row[k] = append([]byte{}, v...)
				if row[k] == nil {
					row[k] = []byte{}
				}
			}
		}
		res.Rows = append(res.Rows, row)
	}
}

func parseField(data []byte) (Field, error) {
	var f Field
	i := 0
	var strs [6][]byte
	for k := range strs {
		v, next, _, err := readLenencBytes(data, i)
		if err != nil {
			return f, err
		}
		strs[k] = v
		i = next
	}
	// catalog, schema, table, org_table, name, org_name
	f.Table = string(strs[2])
	f.Name = string(strs[4])
	_, i, _, err := readLenenc(data, i) // length of fixed fields (0x0c)
	if err != nil || i+10 > len(data) {
		return f, malformed()
	}
	f.Charset = binary.LittleEndian.Uint16(data[i:])
	f.Type = data[i+6]
	f.Flags = binary.LittleEndian.Uint16(data[i+7:])
	return f, nil
}

// --- value conversion (PyMySQL's decoders) ---

// Decimal is a DECIMAL column value (Python decimal.Decimal).
type Decimal string

// Bytes is a binary column value (Python bytes).
type Bytes []byte

// Date, DateTime and Timedelta mirror Python's datetime types; String()
// is Python's str().
type Date struct{ Year, Month, Day int }

type DateTime struct {
	Year, Month, Day, Hour, Minute, Second, Micro int
}

type Timedelta struct{ Micros int64 }

func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day) }

func (d DateTime) String() string {
	s := fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", d.Year, d.Month, d.Day, d.Hour, d.Minute, d.Second)
	if d.Micro != 0 {
		s += fmt.Sprintf(".%06d", d.Micro)
	}
	return s
}

// ISO is datetime.isoformat().
func (d DateTime) ISO() string { return strings.Replace(d.String(), " ", "T", 1) }

func (t Timedelta) String() string {
	us := t.Micros
	days := us / 86400e6
	rem := us % 86400e6
	if rem < 0 {
		rem += 86400e6
		days--
	}
	secs := rem / 1e6
	micro := rem % 1e6
	s := fmt.Sprintf("%d:%02d:%02d", secs/3600, secs/60%60, secs%60)
	if micro != 0 {
		s += fmt.Sprintf(".%06d", micro)
	}
	if days != 0 {
		plural := "s"
		if days == 1 || days == -1 {
			plural = ""
		}
		s = fmt.Sprintf("%d day%s, %s", days, plural, s)
	}
	return s
}

var (
	datetimeRe  = regexp.MustCompile(`^(\d{1,4})-(\d{1,2})-(\d{1,2})[T ](\d{1,2}):(\d{1,2}):(\d{1,2})(?:.(\d{1,6}))?$`)
	timedeltaRe = regexp.MustCompile(`^(-)?(\d{1,3}):(\d{1,2}):(\d{1,2})(?:.(\d{1,6}))?$`)
	dateRe      = regexp.MustCompile(`^(\d{1,4})-(\d{1,2})-(\d{1,2})$`)
)

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func fraction(s string) int {
	if s == "" {
		return 0
	}
	for len(s) < 6 {
		s += "0"
	}
	return atoi(s[:6])
}

func validDate(y, m, d int) bool {
	if y < 1 || m < 1 || m > 12 || d < 1 {
		return false
	}
	days := [...]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[m-1]
	if m == 2 && (y%4 == 0 && (y%100 != 0 || y%400 == 0)) {
		days = 29
	}
	return d <= days
}

// Convert decodes a text-protocol cell the way PyMySQL does: integers to
// int64 (uint64 beyond), floats to float64, DECIMAL to Decimal, temporal
// types to Date/DateTime/Timedelta (unparseable ones stay strings), binary
// strings to Bytes and everything else to string. NULL is nil.
func (f Field) Convert(v []byte) any {
	if v == nil {
		return nil
	}
	s := string(v)
	switch f.Type {
	case TypeTiny, TypeShort, TypeLong, TypeLongLong, TypeInt24, TypeYear:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			return n
		}
		return s
	case TypeFloat, TypeDouble:
		if x, err := strconv.ParseFloat(s, 64); err == nil {
			return x
		}
		return s
	case TypeDecimal, TypeNewDecimal:
		return Decimal(s)
	case TypeTimestamp, TypeDatetime:
		if m := datetimeRe.FindStringSubmatch(s); m != nil {
			d := DateTime{atoi(m[1]), atoi(m[2]), atoi(m[3]), atoi(m[4]), atoi(m[5]), atoi(m[6]), fraction(m[7])}
			if validDate(d.Year, d.Month, d.Day) && d.Hour < 24 && d.Minute < 60 && d.Second < 60 {
				return d
			}
			return s
		}
		if m := dateRe.FindStringSubmatch(s); m != nil && validDate(atoi(m[1]), atoi(m[2]), atoi(m[3])) {
			return Date{atoi(m[1]), atoi(m[2]), atoi(m[3])}
		}
		return s
	case TypeDate:
		if m := dateRe.FindStringSubmatch(s); m != nil && validDate(atoi(m[1]), atoi(m[2]), atoi(m[3])) {
			return Date{atoi(m[1]), atoi(m[2]), atoi(m[3])}
		}
		return s
	case TypeTime:
		if m := timedeltaRe.FindStringSubmatch(s); m != nil {
			us := (int64(atoi(m[2]))*3600+int64(atoi(m[3]))*60+int64(atoi(m[4])))*1e6 + int64(fraction(m[5]))
			if m[1] == "-" {
				us = -us
			}
			return Timedelta{us}
		}
		return s
	case TypeBit, TypeGeometry:
		return Bytes(v)
	case TypeJSON:
		return s
	case TypeBlob, TypeTinyBlob, TypeMediumBlob, TypeLongBlob, TypeString, TypeVarString, TypeVarchar:
		if f.Charset == binaryCharset {
			return Bytes(v)
		}
		return strings.ToValidUTF8(s, "�")
	}
	return strings.ToValidUTF8(s, "�")
}
