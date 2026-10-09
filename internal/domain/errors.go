package domain

import (
	"errors"
	"strings"
)

// Sentinel errors. Handlers use errors.Is to select the HTTP status.
var (
	ErrNotFound        = errors.New("not found")
	ErrForbidden       = errors.New("forbidden")
	ErrUnauthenticated = errors.New("not signed in")
	ErrConflict        = errors.New("conflict")
)

// FieldError is a problem with one input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError contains all problems with an input.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Message)
	}
	return "invalid input: " + strings.Join(parts, "; ")
}

// Add records a problem with a field.
func (e *ValidationError) Add(field, message string) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: message})
}

// Err returns nil if there are no problems, otherwise it returns e.
func (e *ValidationError) Err() error {
	if len(e.Fields) == 0 {
		return nil
	}
	return e
}

// Invalid returns a validation error for one field.
func Invalid(field, message string) error {
	return &ValidationError{Fields: []FieldError{{Field: field, Message: message}}}
}

// FieldMessage returns the first message for a field, or "".
// Templates use it to show errors next to the related input.
func (e *ValidationError) FieldMessage(field string) string {
	for _, f := range e.Fields {
		if f.Field == field {
			return f.Message
		}
	}
	return ""
}
