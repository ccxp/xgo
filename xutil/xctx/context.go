// ValueCtx carries a key-value pair like context.
package xctx

import (
	"errors"
	"fmt"
)

// 添加一个新key，返回一个新的ValueCtx
func WithValue(parent *ValueCtx, key, val interface{}) *ValueCtx {
	if key == nil {
		panic("nil key")
	}
	return &ValueCtx{parent, key, val}
}

// A ValueCtx carries a key-value pair. It implements Value for that key and
// delegates all other calls to the embedded ValueCtx.
type ValueCtx struct {
	parent   *ValueCtx
	key, val interface{}
}

func (c *ValueCtx) String() string {
	return fmt.Sprintf("%v.WithValue(%#v, %#v)", c.parent, c.key, c.val)
}

func (c *ValueCtx) Value(key interface{}) interface{} {
	if c.key != nil && key != nil && c.key == key {
		return c.val
	}
	if c.parent == nil {
		return nil
	}
	return c.parent.Value(key)
}

var NilError = errors.New("nil key")

// 添加一个新key，跟WithValue不一样的地方是不返回一个新的ValueCtx.
func (c *ValueCtx) Add(key, val interface{}) error {
	if key == nil {
		return NilError
	}

	parent := &ValueCtx{c.parent, c.key, c.val}

	c.parent = parent
	c.key = key
	c.val = val

	return nil
}
