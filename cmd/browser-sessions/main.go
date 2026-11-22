// Command browser-sessions keeps browser tabs alive independently of agent workers.
package main

import (
	"cmp"
	"log"
	"net/http"
	"os"
	"time"

	"nomanqureshi.dev/hpyo-agent/internal/dotenv"
	"nomanqureshi.dev/hpyo-agent/internal/tool"
)

func main() {
	dotenv.Load(".env")
	token := os.Getenv("OBSCURA_CDP_TOKEN")
	if token == "" {
		log.Fatal("OBSCURA_CDP_TOKEN is required")
	}
	addr := cmp.Or(os.Getenv("BROWSER_SESSION_LISTEN"), "127.0.0.1:9233")
	cdpURL := cmp.Or(os.Getenv("OBSCURA_URL"), "http://127.0.0.1:9222")
	server := http.Server{
		Addr: addr, Handler: tool.BrowserService(cdpURL, token),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 2 * time.Minute, IdleTimeout: 30 * time.Second,
	}
	log.Printf("browser session service listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}
