// ini config file loader.
package config

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	defaultSection = "default"   // default section means if some ini items not in a section, make them in default section,
	bNumComment    = []byte{'#'} // number signal
	bSemComment    = []byte{';'} // semicolon signal
	bEmpty         = []byte{}
	bEqual         = []byte{'='} // equal signal
	bDQuote        = []byte{'"'} // quote signal
	sectionStart   = []byte{'['} // section start signal
	sectionEnd     = []byte{']'} // section end signal
	lineBreak      = "\n"
)

// parse ini file.
func Parse(fn string) (*IniConfig, error) {
	cfg := &IniConfig{
		fn:     fn,
		useEnv: false,
		data:   make(map[string]map[string]string),
	}
	err := cfg.Parse(fn)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// parse ini file.
//
// Value like $xxx, ${xxx] convert with environment variable.
func ParseUseEnv(fn string) (*IniConfig, error) {
	cfg := &IniConfig{
		fn:     fn,
		useEnv: true,
		data:   make(map[string]map[string]string),
	}
	err := cfg.Parse(fn)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func ParseData(data []byte, useEnv bool) (*IniConfig, error) {
	cfg := &IniConfig{
		useEnv: useEnv,
		data:   make(map[string]map[string]string),
	}
	err := cfg.parseData(data)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// IniConfig implements Config to parse ini file.
type IniConfig struct {
	fn     string
	useEnv bool
	data   map[string]map[string]string // section=> key:val
}

func (ini *IniConfig) File() string {
	return ini.fn
}

// Parse creates a new Config and parses the file configuration from the named file.
func (ini *IniConfig) Parse(fn string) error {
	return ini.parseFile(fn)
}

func (ini *IniConfig) parseFile(fn string) error {
	data, err := ioutil.ReadFile(fn)
	if err != nil {
		return err
	}

	return ini.parseData(data)
}

func (ini *IniConfig) parseData(data []byte) error {

	buf := bufio.NewReader(bytes.NewBuffer(data))
	// check the BOM
	head, err := buf.Peek(3)
	if err == nil && head[0] == 239 && head[1] == 187 && head[2] == 191 {
		for i := 1; i <= 3; i++ {
			buf.ReadByte()
		}
	}
	section := defaultSection
	for {
		line, _, err := buf.ReadLine()
		if err == io.EOF {
			break
		}
		//It might be a good idea to throw a error on all unknonw errors?
		if _, ok := err.(*os.PathError); ok {
			return err
		}
		line = bytes.TrimSpace(line)
		if bytes.Equal(line, bEmpty) || bytes.HasPrefix(line, bNumComment) || bytes.HasPrefix(line, bSemComment) {
			continue
		}

		if bytes.HasPrefix(line, sectionStart) && bytes.HasSuffix(line, sectionEnd) {
			section = strings.ToLower(string(line[1 : len(line)-1])) // section name case insensitive
			if _, ok := ini.data[section]; !ok {
				ini.data[section] = make(map[string]string)
			}
			continue
		}

		if _, ok := ini.data[section]; !ok {
			ini.data[section] = make(map[string]string)
		}
		keyValue := bytes.SplitN(line, bEqual, 2)

		key := string(bytes.TrimSpace(keyValue[0])) // key name case insensitive
		key = strings.ToLower(key)

		if len(keyValue) != 2 {
			ini.data[section][key] = ""
			continue
		}
		val := bytes.TrimSpace(keyValue[1])
		if bytes.HasPrefix(val, bDQuote) {
			val = bytes.Trim(val, `"`)
		}

		if ini.useEnv {
			ini.data[section][key] = getEnvValue(string(val))
		} else {
			ini.data[section][key] = string(val)
		}

	}
	return nil
}

// Bool returns the boolean value for a given key.
func (c *IniConfig) GetBool(section string, key string, default_value bool) bool {
	s := c.getdata(section, key)
	if s == nil {
		return default_value
	}

	v, err := strconv.ParseBool(*s)
	if err != nil {
		return default_value
	}
	return v
}

// Int returns the integer value for a given key.
func (c *IniConfig) GetInt(section string, key string, default_value int) int {
	s := c.getdata(section, key)
	if s == nil || len(*s) == 0 {
		return default_value
	}

	res := default_value
	S := *s
	end := S[len(S)-1]
	mul := 1
	switch end {
	case 'K', 'k':
		mul = 1 << 10
		S = S[0 : len(S)-1]
	case 'M', 'm':
		mul = 1 << 20
		S = S[0 : len(S)-1]
	case 'G', 'g':
		mul = 1 << 30
		S = S[0 : len(S)-1]
	}
	v, err := strconv.Atoi(S)
	if err == nil {
		res = v * mul
	}
	return res
}

// Int64 returns the int64 value for a given key.
func (c *IniConfig) GetInt64(section string, key string, default_value int64) int64 {
	s := c.getdata(section, key)
	if s == nil || len(*s) == 0 {
		return default_value
	}

	res := default_value
	S := *s
	end := S[len(S)-1]
	var mul int64 = 1
	switch end {
	case 'K', 'k':
		mul = 1 << 10
		S = S[0 : len(S)-1]
	case 'M', 'm':
		mul = 1 << 20
		S = S[0 : len(S)-1]
	case 'G', 'g':
		mul = 1 << 30
		S = S[0 : len(S)-1]
	case 'T', 't':
		mul = 1 << 40
		S = S[0 : len(S)-1]
	}
	v, err := strconv.ParseInt(S, 10, 64)
	if err == nil {
		res = v * mul
	}
	return res
}

func (c *IniConfig) GetFloat(section string, key string, default_value float64) float64 {
	s := c.getdata(section, key)
	if s == nil {
		return default_value
	}

	v, err := strconv.ParseFloat(*s, 64)
	if err != nil {
		return default_value
	}
	return v
}

func (c *IniConfig) GetDuration(section string, key string, default_value time.Duration) time.Duration {
	S := c.GetString(section, key, "")
	if S == "" {
		return default_value
	}

	end := S[len(S)-1]
	mul := int64(1)
	switch end {
	case 'S', 's':
		if len(S) >= 2 && (S[len(S)-2] == 'M' || S[len(S)-2] == 'm') {
			mul = time.Millisecond.Nanoseconds()
			S = S[0 : len(S)-2]
		} else if len(S) >= 2 && (S[len(S)-2] == 'U' || S[len(S)-2] == 'u') {
			mul = time.Microsecond.Nanoseconds()
			S = S[0 : len(S)-2]
		} else {
			mul = time.Second.Nanoseconds()
			S = S[0 : len(S)-1]
		}
	case 'M', 'm':
		mul = time.Minute.Nanoseconds()
		S = S[0 : len(S)-1]
	case 'H', 'h':
		mul = time.Hour.Nanoseconds()
		S = S[0 : len(S)-1]
	case 'D', 'd':
		mul = time.Hour.Nanoseconds() * 24
		S = S[0 : len(S)-1]
	}
	v, err := strconv.ParseInt(S, 0, 64)
	if err == nil {
		return time.Duration(v * mul)
	}
	return default_value
}

// String returns the string value for a given key.
func (c *IniConfig) GetString(section string, key string, default_value string) string {
	s := c.getdata(section, key)
	if s == nil {
		return default_value
	}
	return *s
}

// GetSection returns map for the given section
func (c *IniConfig) GetSection(section string) (map[string]string, error) {
	if v, ok := c.data[strings.ToLower(section)]; ok {
		return v, nil
	}
	return nil, errors.New("not exist section")
}

// section.key or key
func (c *IniConfig) getdata(section string, key string) *string {
	if v, ok := c.data[strings.ToLower(section)]; ok {
		if vv, ok := v[strings.ToLower(key)]; ok {
			return &vv
		}
	}
	return nil
}

// set new value.
func (c *IniConfig) SetValue(section string, key string, value string) {
	s := strings.ToLower(section)
	k := strings.ToLower(key)

	if _, ok := c.data[s]; !ok {
		c.data[s] = make(map[string]string)
	}
	c.data[s][k] = value
}

// 取所有value
func (c *IniConfig) GetValues() map[string]map[string]string {
	ret := make(map[string]map[string]string)
	for k, vv := range c.data {
		ret[k] = make(map[string]string)
		for vk, v := range vv {
			ret[k][vk] = v
		}
	}
	return ret
}

// getEnvValue returns value of convert with environment variable.
//
// Return environment variable if value start with "${" and end with "}".
// Return default value if environment variable is empty or not exist.
//
// It accept value formats "${env}" , "${env||}}" , "${env||defaultValue}" , "defaultvalue".
// Examples:
//	v1 := config.getEnvValue("${GOPATH}")			// return the GOPATH environment variable.
//	v2 := config.getEnvValue("${GOAsta||/usr/local/go}")	// return the default value "/usr/local/go/".
//	v3 := config.getEnvValue("Astaxie")				// return the value "Astaxie".
func getEnvValue(value string) (realValue string) {
	realValue = value

	vLen := len(value)
	// 3 = ${}
	if vLen < 3 {
		return
	}
	// Need start with "${" and end with "}", then return.
	if value[0] != '$' || value[1] != '{' || value[vLen-1] != '}' {
		return
	}

	key := ""
	defalutV := ""
	// value start with "${"
	for i := 2; i < vLen; i++ {
		if value[i] == '|' && (i+1 < vLen && value[i+1] == '|') {
			key = value[2:i]
			defalutV = value[i+2 : vLen-1] // other string is default value.
			break
		} else if value[i] == '}' {
			key = value[2:i]
			break
		}
	}

	realValue = os.Getenv(key)
	if realValue == "" {
		realValue = defalutV
	}

	return
}

var errValue = errors.New("value error")

func ParseDuration(S string) (time.Duration, error) {

	if S == "" {
		return 0, errValue
	}

	end := S[len(S)-1]
	mul := int64(1)
	switch end {
	case 'S', 's':
		if len(S) >= 2 && (S[len(S)-2] == 'M' || S[len(S)-2] == 'm') {
			mul = time.Millisecond.Nanoseconds()
			S = S[0 : len(S)-2]
		} else if len(S) >= 2 && (S[len(S)-2] == 'U' || S[len(S)-2] == 'u') {
			mul = time.Microsecond.Nanoseconds()
			S = S[0 : len(S)-2]
		} else {
			mul = time.Second.Nanoseconds()
			S = S[0 : len(S)-1]
		}
	case 'M', 'm':
		mul = time.Minute.Nanoseconds()
		S = S[0 : len(S)-1]
	case 'H', 'h':
		mul = time.Hour.Nanoseconds()
		S = S[0 : len(S)-1]
	case 'D', 'd':
		mul = time.Hour.Nanoseconds() * 24
		S = S[0 : len(S)-1]
	}
	v, err := strconv.ParseInt(S, 0, 64)
	if err == nil {
		return time.Duration(v * mul), nil
	}
	return 0, err
}
