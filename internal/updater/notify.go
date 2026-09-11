package updater

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/nicholas-fedor/shoutrrr"
)

var urlRE = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"']+`)

func redact(s string) string {
	return urlRE.ReplaceAllString(s, "<url>")
}

// LoadURLs reads Shoutrrr URLs, one per line. Blank lines and # comments are
// skipped. A missing file means no notes.
func LoadURLs(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			urls = append(urls, l)
		}
	}
	return urls, nil
}

// Sender returns a notify func that sends msg to every URL. A failed note is
// logged only; it never blocks an update. URLs hold tokens, so they are
// never logged.
func Sender(urls []string) func(string) {
	return func(msg string) {
		log.Printf("note: %s", msg)
		for _, u := range urls {
			if err := shoutrrr.Send(u, msg); err != nil {
				scheme, _, _ := strings.Cut(u, ":")
				log.Printf("note to %s failed: %s", scheme, redact(err.Error()))
			}
		}
	}
}
