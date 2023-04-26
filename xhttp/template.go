package xhttp

import (
	"github.com/ccxp/xgo/xlog"
	"html/template"
	"io/ioutil"
	"sync"
)

type TemplateLoader struct {
	BaseDir     string // base file directory
	EnableCache bool   // store *template in loader

	FuncMap template.FuncMap

	mapTpl map[string]*template.Template
	muTpl  sync.RWMutex
}

// Read file and Parse into *template.Template.
// File path = l.BaseDir + name.
func (l *TemplateLoader) Get(name string) *template.Template {

	path := l.BaseDir + name
	var t *template.Template
	if l.EnableCache {
		ok := false
		l.muTpl.RLock()
		if l.mapTpl != nil {
			t, ok = l.mapTpl[name]
		}
		l.muTpl.RUnlock()
		if ok {
			return t
		}
	}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		xlog.Errorf("read template file %s fail: %v", path, err)
		return nil
	}

	if l.FuncMap != nil {
		t = template.Must(template.New(name).Funcs(l.FuncMap).Parse(string(data)))
	} else {
		t = template.Must(template.New(name).Parse(string(data)))
	}
	if l.EnableCache {
		l.muTpl.Lock()
		if l.mapTpl == nil {
			l.mapTpl = make(map[string]*template.Template)
		}
		l.mapTpl[name] = t
		l.muTpl.Unlock()
	}

	return t
}
