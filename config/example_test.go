package config_test

import (
	"fmt"
	"os"

	"github.com/ccxp/xgo/config"
)

func ExampleParse() {
	c, err := config.Parse("./test.conf")
	if err != nil {
		return
	}

	fmt.Fprintf(os.Stderr, "%v\n", c.GetInt64("a", "b", 6))
	fmt.Fprintf(os.Stderr, "%v\n", c.GetInt64("a", "c", 7))
	fmt.Fprintf(os.Stderr, "%v\n", c.GetInt64("a", "d", 8))
	fmt.Fprintf(os.Stderr, "%v\n", c.GetInt64("a", "e", 8))

	fmt.Fprintf(os.Stderr, "%v\n", c.GetInt("a", "b", 6))
	fmt.Fprintf(os.Stderr, "%v\n", c.GetInt("a", "c", 7))
	fmt.Fprintf(os.Stderr, "%v\n", c.GetInt("a", "d", 8))
	fmt.Fprintf(os.Stderr, "%v\n", c.GetDuration("c", "f", 0))
	fmt.Fprintf(os.Stderr, "%v\n", c.GetDuration("c", "g", 0))

	// Output: 2
	// 2048
	// 2097152
	// 2199023255552
	// 2
	// 2048
	// 2097152
	// 1ms
	// 10µs
}
