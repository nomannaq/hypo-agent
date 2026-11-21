// Package dotenv loads .env for binaries started without `make`.
package dotenv

import (
	"bufio"
	"os"
	"strings"
)

// Load sets variables from the file at path that are not already in the
// environment, so a binary also works when started without `make`. A key
// repeated in the file takes its last value, as make's `include` and Docker
// Compose do; otherwise the worker and the containers can disagree (a
// duplicated OBSCURA_CDP_TOKEN line once meant every browse got a 401).
func Load(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	vars := map[string]string{}
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
		vars[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	for k, v := range vars {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}
