// Command mint-llm-token prints an LLM gateway task token (for hack/check-gemini-gateway.sh):
//
//	go run ./hack/mint-llm-token <secret> <task id> <ttl, e.g. 1h>
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/llmgateway"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: mint-llm-token <secret> <task id> <ttl>")
		os.Exit(2)
	}
	ttl, err := time.ParseDuration(os.Args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println(llmgateway.Token(os.Args[1], os.Args[2], time.Now().Add(ttl)))
}
