// Command usend is the uSend command-line tool: send email, inspect delivery,
// and manage domains, webhooks and API keys from a terminal or a script.
package main

import (
	"os"

	"usend.et/cli/internal/app"
)

func main() {
	os.Exit(app.Main(os.Args[1:], app.DefaultEnv()))
}
