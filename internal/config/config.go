// Package config reads the configuration from environment variables.
// For each secret value NAME, the variable NAME_FILE can give the path of
// a file that contains the value. Kubernetes mounts secrets as files, thus
// secrets do not show in the process environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Reader reads variables and collects all errors.
type Reader struct {
	errs []error
	// Lookup returns the value of a variable. The default is os.LookupEnv.
	Lookup func(string) (string, bool)
}

func (r *Reader) lookup(name string) (string, bool) {
	if r.Lookup != nil {
		return r.Lookup(name)
	}
	return os.LookupEnv(name)
}

// Err returns all errors.
func (r *Reader) Err() error { return errors.Join(r.errs...) }

func (r *Reader) fail(format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf(format, args...))
}

// String returns a variable, or def if it is not set.
func (r *Reader) String(name, def string) string {
	if v, ok := r.lookup(name); ok && v != "" {
		return v
	}
	return def
}

// Required returns a variable and records an error if it is not set.
func (r *Reader) Required(name string) string {
	v := r.String(name, "")
	if v == "" {
		r.fail("%s is required", name)
	}
	return v
}

// Secret returns the value of name, or the content of the file in
// name_FILE. It records an error if required and neither is set.
func (r *Reader) Secret(name string, required bool) string {
	if path := r.String(name+"_FILE", ""); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			r.fail("%s_FILE: %v", name, err)
			return ""
		}
		return strings.TrimRight(string(b), "\r\n")
	}
	v := r.String(name, "")
	if v == "" && required {
		r.fail("%s or %s_FILE is required", name, name)
	}
	return v
}

// Bool returns a boolean variable.
func (r *Reader) Bool(name string, def bool) bool {
	v := r.String(name, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.fail("%s: %q is not true or false", name, v)
		return def
	}
	return b
}

// Int returns an integer variable.
func (r *Reader) Int(name string, def int) int {
	v := r.String(name, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.fail("%s: %q is not a whole number", name, v)
		return def
	}
	return n
}

// Duration returns a duration variable, for example "30s".
func (r *Reader) Duration(name string, def time.Duration) time.Duration {
	v := r.String(name, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		r.fail("%s: %q is not a positive duration", name, v)
		return def
	}
	return d
}
