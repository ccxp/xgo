// time json formatter.
package xtime

import (
	"errors"
	"strconv"
	"time"
)

// Time with self define json formater.
//
// default layout is time.RFC3339.
//
// if layout is "1136239445", output as int64.
//
//	time.RFC3339     = "2006-01-02T15:04:05Z07:00"
//	xtime.UnixTimeLayout =  "1136239445"
//
type CustomTime struct {
	T               time.Time
	unmarshalLayout string
	marshalLayout   string
}

func CustomTimeNow() CustomTime {
	return CustomTime{
		T: time.Now(),
	}
}

// output use time.Unix()
const UnixTimeLayout = "1136239445"

var customTimeUnmarshalLayout = time.RFC3339
var customTimeMarshalLayout = time.RFC3339

// set global unmarshal layout
func SetCustomTimeUnmarshalLayout(layout string) {
	customTimeUnmarshalLayout = layout
}

// set global marshal layout
func SetCustomTimeMarshalLayout(layout string) {
	customTimeMarshalLayout = layout
}

func (t *CustomTime) SetUnmarshalLayout(layout string) {
	t.unmarshalLayout = layout
}

func (t *CustomTime) SetMarshalLayout(layout string) {
	t.marshalLayout = layout
}

// UnmarshalJSON implements the json.Unmarshaler interface.
func (t *CustomTime) UnmarshalJSON(data []byte) error {
	// Ignore null, like in the main JSON package.
	if string(data) == "null" {
		return nil
	}

	layout := t.unmarshalLayout
	if layout == "" {
		layout = customTimeUnmarshalLayout
	}

	if layout == UnixTimeLayout {
		v, err := strconv.ParseInt(string(data), 10, 64)
		if err != nil {
			return err
		}
		t.T = time.Unix(v, 0)
		return nil
	}

	// Fractional seconds are handled implicitly by Parse.
	var err error
	t.T, err = time.Parse(`"`+layout+`"`, string(data))
	return err
}

// MarshalJSON implements the json.Marshaler interface.
func (t CustomTime) MarshalJSON() ([]byte, error) {
	if y := t.T.Year(); y < 0 || y >= 10000 {
		// RFC 3339 is clear that years are 4 digits exactly.
		// See golang.org/issue/4556#c15 for more discussion.
		return nil, errors.New("Time.MarshalJSON: year outside of range [0,9999]")
	}

	layout := t.marshalLayout
	if layout == "" {
		layout = customTimeMarshalLayout
	}

	if layout == UnixTimeLayout {
		return []byte(strconv.FormatInt(t.T.Unix(), 10)), nil
	}

	const bufSize = 68
	max := len(layout) + 12
	if max < bufSize {
		max = bufSize
	}

	b := make([]byte, 0, max)
	b = append(b, '"')
	b = t.T.AppendFormat(b, layout)
	b = append(b, '"')
	return b, nil
}

func (t CustomTime) String() string {
	return t.T.String()
}
