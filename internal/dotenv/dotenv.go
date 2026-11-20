// Package dotenv loads .env for binaries started without `make`.
package dotenv

import (
	"bufio"
	"os"
	"strings"
)

// Load sets variables from the file at path that are not already in the
// environment, so a binary also works when started without `make`.
func Load(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func(f *os.File) {
		err := f.Close()
		if err != nil {

		}
	}(f)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set || os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}
