package xhttp

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Values maps a string key to a list of values.
// It is typically used for query parameters and form values.
//
// Is the same as net/url.Values
type Values map[string][]string

// Decode Values into struct, *map[string]string or *map[string][]string.
//
// Struct field can be customized by the format string stored under the "query" key in the struct field's tag.
//
// Example:
//
//     type MyData struct {
//         Aa int      `query:"aa"`
//         Bb []string `query:"bb"`
//         Cc *int     `query:"cc"`
//         Dd []*int   `query:"dd"`
//     }
//
// If dat is *map[string]string, only store the first value in the same key.
func (v *Values) Unmarshal(dat interface{}) error {

	switch dat.(type) {
	case *map[string]string:
		var m map[string][]string = *v
		for k, s := range m {
			if len(s) > 0 {
				(*dat.(*map[string]string))[k] = s[0]
			}
		}
		return nil
	case *map[string][]string:
		var m map[string][]string = *v
		for k, s := range m {
			(*dat.(*map[string][]string))[k] = s
		}
		return nil
	}

	ptr := reflect.ValueOf(dat)

	if !ptr.IsValid() {
		return errors.New("v is not valid input")
	}
	if ptr.Kind() != reflect.Ptr {
		return errors.New("not ptr input")
	}
	if ptr.IsNil() {
		return errors.New("v is nil")
	}

	pv := ptr.Elem()
	if pv.Kind() != reflect.Struct {
		return errors.New("v is not struct")
	}

	var isSlice bool
	var k reflect.Kind
	var err error
	for i := 0; i < pv.NumField(); i++ {
		f := pv.Field(i)
		if !f.CanSet() {
			continue
		}

		k = f.Kind()
		if k == reflect.Struct {
			err = v.Unmarshal(pv.Field(i).Addr().Interface())
			if err != nil {
				return err
			}
			continue
		}

		name := pv.Type().Field(i).Tag.Get("query")
		if name == "" {
			name = pv.Type().Field(i).Name
		}
		vs, ok := (*v)[name]
		if !ok {
			continue
		}

		if k == reflect.Slice {
			isSlice = true
			k = f.Type().Elem().Kind()
		} else {
			isSlice = false
		}

		if isSlice {
			f.Set(reflect.MakeSlice(f.Type(), len(vs), len(vs)))
			for j := 0; j < len(vs); j++ {
				err = v.assignValue(vs[j], f.Index(j))
				if err != nil {
					return err
				}
			}
		} else if len(vs) > 0 {
			err = v.assignValue(vs[0], f)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (v *Values) assignValue(s string, f reflect.Value) error {
	if f.Kind() == reflect.Ptr {
		f.Set(reflect.New(f.Type().Elem()))
		f = f.Elem()
	}

	switch f.Kind() {
	case reflect.Bool:
		if s == "" || s == "0" || strings.ToLower(s) == "false" {
			f.SetBool(false)
		} else {
			f.SetBool(true)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if s == "" {
			f.SetInt(0)
			return nil
		}

		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		if f.OverflowInt(n) {
			return fmt.Errorf("type [%v] value [%s] overflow", f.Kind(), s)
		}
		f.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if s == "" {
			f.SetUint(0)
			return nil
		}

		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return err
		}
		if f.OverflowUint(n) {
			return fmt.Errorf("type [%v] value [%s] overflow", f.Kind(), s)
		}
		f.SetUint(n)
	case reflect.Float32, reflect.Float64:
		if s == "" {
			f.SetFloat(0)
			return nil
		}

		n, err := strconv.ParseFloat(s, f.Type().Bits())
		if err != nil {
			return err
		}
		if f.OverflowFloat(n) {
			return fmt.Errorf("type [%v] value [%s] overflow", f.Kind(), s)
		}
		f.SetFloat(n)
	case reflect.String:
		f.SetString(s)
	default:
		return fmt.Errorf("field type [%v] not allowed", f.Kind())
	}
	return nil
}

// Encode struct, map[string]string, map[string][]string, *map[string]string or *map[string][]string into Values.
//
// Struct field can be customized by the format string stored under the "query" key in the struct field's tag.
//
// Example:
//
//     type MyData struct {
//         Aa int      `query:"aa"`
//         Bb []string `query:"bb"`
//         Cc *int     `query:"cc"`
//         Dd []*int   `query:"dd"`
//     }
//
func (v *Values) Marshal(dat interface{}) error {

	if dat == nil {
		return nil
	}

	switch dat.(type) {
	case map[string]string:
		for k, s := range dat.(map[string]string) {
			(*v)[k] = []string{s}
		}
		return nil
	case map[string][]string:
		for k, s := range dat.(map[string][]string) {
			(*v)[k] = s
		}
		return nil
	case *map[string]string:
		for k, s := range *(dat.(*map[string]string)) {
			(*v)[k] = []string{s}
		}
		return nil
	case *map[string][]string:
		for k, s := range *(dat.(*map[string][]string)) {
			(*v)[k] = s
		}
		return nil
	default:

	}

	ptr := reflect.ValueOf(dat)
	var pv reflect.Value

	if !ptr.IsValid() {
		return errors.New("dat is not valid")
	}
	if ptr.Kind() == reflect.Ptr {
		if ptr.IsNil() {
			return errors.New("dat is nil")
		}
		pv = ptr.Elem()
	} else {
		pv = ptr
	}

	if pv.Kind() != reflect.Struct {
		return errors.New("dat is not struct")
	}

	var isSlice bool
	var k reflect.Kind
	var err error

	for i := 0; i < pv.NumField(); i++ {
		f := pv.Field(i)
		if pv.Type().Field(i).Anonymous || pv.Type().Field(i).PkgPath != "" {
			continue
		}

		k = f.Kind()
		name := pv.Type().Field(i).Tag.Get("query")
		if name == "-" {
			continue
		}
		if name == "" {
			name = pv.Type().Field(i).Name
		}

		cnt := 1
		if k == reflect.Slice {
			isSlice = true
			cnt = f.Len()
		} else {
			isSlice = false
		}

		vs := make([]string, cnt)
		err = nil
		if isSlice {
			var s string
			vidx := 0
			for j := 0; j < cnt; j++ {
				s, err = v.fieldValue(f.Index(j))
				if err == nil {
					vs[vidx] = s
					vidx++
				}
			}
			if vidx > 0 {
				(*v)[name] = vs[0:vidx]
			}
		} else {
			vs[0], err = v.fieldValue(f)
			if err == nil {
				(*v)[name] = vs
			}
		}
	}

	return nil
}

var errNilPtr = errors.New("field is nil ptr")

func (v *Values) fieldValue(f reflect.Value) (string, error) {
	if f.Kind() == reflect.Ptr {
		if f.IsNil() {
			return "", errNilPtr
		}
		f = f.Elem()
	}

	switch f.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(f.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(f.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return fmt.Sprintf("%f", f.Float()), nil
	case reflect.String:
		return f.String(), nil
	}
	return "", fmt.Errorf("field type [%v] not allowed", f.Kind())
}
