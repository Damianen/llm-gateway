// Command fakeupstream serves canned Anthropic and OpenAI-compatible
// endpoints for scripts/smoke.sh and local experimentation. It is a test
// double — never deploy it.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/Damianen/llm-gateway/internal/upstreamfake"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9101", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	// Base URL conventions match the adapters: the anthropic adapter appends
	// /v1/messages to its base, the openai adapter appends /chat/completions.
	mux.Handle("POST /anthropic/v1/messages", upstreamfake.NewAnthropic())
	mux.Handle("POST /openai/v1/chat/completions", upstreamfake.NewOpenAI())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	log.Printf("fakeupstream listening on %s (anthropic base: http://%s/anthropic, openai base: http://%s/openai/v1)",
		*listen, *listen, *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		fmt.Fprintln(os.Stderr, "fakeupstream:", err)
		os.Exit(1)
	}
}
