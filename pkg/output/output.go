package output

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"gopkg.in/yaml.v3"
)

// Format represents an output format type.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

// Writer writes structured output in the requested format.
type Writer struct {
	w      io.Writer
	format Format
}

// NewWriter creates a new output writer with the given format.
func NewWriter(w io.Writer, format string) *Writer {
	f := FormatTable
	switch format {
	case "json":
		f = FormatJSON
	case "yaml":
		f = FormatYAML
	default:
		f = FormatTable
	}
	return &Writer{w: w, format: f}
}

// Table returns a tabwriter.Writer if the format is table, otherwise an io.Discard wrapper.
func (ow *Writer) Table() io.Writer {
	if ow.format != FormatTable {
		return io.Discard
	}
	return tabwriter.NewWriter(ow.w, 0, 0, 2, ' ', 0)
}

// FlushTable flushes the table writer if it was used. Call after all rows are written.
func (ow *Writer) FlushTable(tw io.Writer) {
	if w, ok := tw.(*tabwriter.Writer); ok {
		w.Flush()
	}
}

// PrintJSON marshals value as indented JSON to the output.
func (ow *Writer) PrintJSON(v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}
	_, err = fmt.Fprintln(ow.w, string(data))
	return err
}

// PrintYAML marshals value as YAML to the output.
func (ow *Writer) PrintYAML(v interface{}) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("failed to marshal YAML: %w", err)
	}
	_, err = ow.w.Write(data)
	return err
}
