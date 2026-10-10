// Command ssoclient signs in to web applications through Keycloak like a
// browser. The end-to-end test uses it. Curl cannot do this: Keycloak sets
// Secure cookies, and curl accepts Secure cookies over plain HTTP only for
// "localhost", not for "keycloak.localhost". Browsers and the Go cookie jar
// accept them for all *.localhost names.
//
// Usage:
//
//	ssoclient -user NAME -password-file FILE STEP...
//
// Each STEP is one of:
//
//	login=URL         open URL; if Keycloak shows its sign-in form, send it
//	get=URL           send a GET request with the cookies of the session
//	cookies=URL,FILE  write the cookies for URL to FILE in the curl format
//
// The program writes one JSON line for each step. It stops with exit
// status 1 if a step fails.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var formAction = regexp.MustCompile(`action="([^"]*login-actions/authenticate[^"]*)"`)

func main() {
	user := flag.String("user", "", "Keycloak user name")
	passwordFile := flag.String("password-file", "", "file with the password")
	flag.Parse()
	if err := run(*user, *passwordFile, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "ssoclient:", err)
		os.Exit(1)
	}
}

func run(user, passwordFile string, steps []string) error {
	b, err := os.ReadFile(passwordFile)
	if err != nil {
		return err
	}
	password := strings.TrimRight(string(b), "\r\n")
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	c := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	out := json.NewEncoder(os.Stdout)
	for _, step := range steps {
		kind, arg, ok := strings.Cut(step, "=")
		if !ok {
			return fmt.Errorf("bad step %q", step)
		}
		switch kind {
		case "login":
			final, form, err := login(c, arg, user, password)
			if err != nil {
				return fmt.Errorf("login at %s: %w", arg, err)
			}
			_ = out.Encode(map[string]any{"login": arg, "final": final, "form": form})
		case "get":
			resp, err := c.Get(arg)
			if err != nil {
				return err
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			var v any
			if json.Unmarshal(body, &v) != nil {
				v = string(body)
			}
			_ = out.Encode(map[string]any{"get": arg, "status": resp.StatusCode, "body": v})
		case "cookies":
			target, file, ok := strings.Cut(arg, ",")
			if !ok {
				return fmt.Errorf("bad step %q", step)
			}
			if err := writeCookies(jar, target, file); err != nil {
				return err
			}
			_ = out.Encode(map[string]any{"cookies": target, "file": file})
		default:
			return fmt.Errorf("unknown step %q", kind)
		}
	}
	return nil
}

// login opens start and sends the Keycloak form if Keycloak shows it. It
// returns the address of the last page and tells if a form was sent.
func login(c *http.Client, start, user, password string) (string, bool, error) {
	resp, err := c.Get(start)
	if err != nil {
		return "", false, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	m := formAction.FindSubmatch(body)
	if m == nil {
		return resp.Request.URL.String(), false, nil // The Keycloak session is still valid.
	}
	action := html.UnescapeString(string(m[1]))
	resp, err = c.PostForm(action, url.Values{"username": {user}, "password": {password}})
	if err != nil {
		return "", true, err
	}
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if formAction.Match(body) || strings.Contains(resp.Request.URL.Path, "/login-actions/") {
		return "", true, errors.New("Keycloak did not accept the sign-in")
	}
	return resp.Request.URL.String(), true, nil
}

// writeCookies writes the cookies that the jar sends to target in the
// Netscape format that curl reads.
func writeCookies(jar *cookiejar.Jar, target, file string) error {
	u, err := url.Parse(target)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Netscape HTTP Cookie File\n")
	for _, ck := range jar.Cookies(u) {
		// Curl accepts cookies with the __Host- and __Secure- prefixes only
		// with the Secure flag. For localhost, curl also sends Secure
		// cookies over plain HTTP.
		secure := "FALSE"
		if strings.HasPrefix(ck.Name, "__Host-") || strings.HasPrefix(ck.Name, "__Secure-") {
			secure = "TRUE"
		}
		fmt.Fprintf(&b, "%s\tFALSE\t/\t%s\t0\t%s\t%s\n", u.Hostname(), secure, ck.Name, ck.Value)
	}
	return os.WriteFile(file, []byte(b.String()), 0o600)
}
