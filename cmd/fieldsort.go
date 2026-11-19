package cmd

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// This file provides generic --fields/--sort-by support shared by every
// command whose output has a flat, per-row shape (one struct per output
// row, with "json"/"toon" struct tags giving each column its ID) — html5-apps
// was the first command built this way; these helpers generalize it via Go
// generics so every other command reuses the same logic instead of each
// re-implementing its own copy.
//
// For a command whose primary output nests rows under region/org/space (most
// of them), these helpers operate on a separate, purpose-built flat row type
// that mirrors the command's own CSV columns — --fields/--sort-by then apply
// to that flat view; the nested TOON/JSON structure is unaffected unless the
// caller explicitly chooses to render the flat view there too (see each
// command's own flattenXRows + RunE wiring).

// fieldIDsOf returns T's own JSON field names, in struct declaration order —
// the canonical column list used for CSV headers and to validate --fields/
// --sort-by.
func fieldIDsOf[T any]() []string {
	t := reflect.TypeOf((*T)(nil)).Elem()
	ids := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		ids = append(ids, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
	}
	return ids
}

// parseFieldsFlag validates raw's comma-separated column IDs
// case-insensitively against T's own fieldIDsOf(), preserving the order
// given. Returns nil (meaning "every field, in canonical order") when raw is
// empty. flagName is used in the error message (e.g. "--fields").
func parseFieldsFlag[T any](raw, flagName string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	allIDs := fieldIDsOf[T]()
	valid := make(map[string]string, len(allIDs))
	for _, id := range allIDs {
		valid[strings.ToLower(id)] = id
	}
	out := make([]string, 0, len(allIDs))
	for _, f := range splitCSV(raw) {
		id, ok := valid[strings.ToLower(strings.TrimSpace(f))]
		if !ok {
			return nil, fmt.Errorf("invalid %s value %q: must be one of %s", flagName, f, strings.Join(allIDs, ", "))
		}
		out = append(out, id)
	}
	return out, nil
}

// sortKey is one --sort-by criterion: a column ID plus direction.
type sortKey struct {
	ID   string
	Desc bool
}

// parseSortByFlag validates raw's comma-separated column IDs
// case-insensitively against T's own fieldIDsOf(), preserving the order
// given as the tie-breaking priority. Each column may be suffixed with
// ":desc" or ":asc" (":asc" is the default). Returns nil (meaning "no
// explicit sort; the command's own default order applies") when raw is
// empty.
func parseSortByFlag[T any](raw, flagName string) ([]sortKey, error) {
	if raw == "" {
		return nil, nil
	}
	allIDs := fieldIDsOf[T]()
	valid := make(map[string]string, len(allIDs))
	for _, id := range allIDs {
		valid[strings.ToLower(id)] = id
	}
	keys := make([]sortKey, 0, len(allIDs))
	for _, f := range splitCSV(raw) {
		f = strings.TrimSpace(f)
		field, dir := f, ""
		if idx := strings.LastIndex(f, ":"); idx >= 0 {
			field = strings.TrimSpace(f[:idx])
			dir = strings.ToLower(strings.TrimSpace(f[idx+1:]))
		}
		id, ok := valid[strings.ToLower(field)]
		if !ok {
			return nil, fmt.Errorf("invalid %s value %q: must be one of %s, each optionally suffixed with \":desc\" or \":asc\"", flagName, f, strings.Join(allIDs, ", "))
		}
		var desc bool
		switch dir {
		case "", "asc":
			desc = false
		case "desc":
			desc = true
		default:
			return nil, fmt.Errorf("invalid %s direction %q for field %q: must be \"asc\" or \"desc\"", flagName, dir, id)
		}
		keys = append(keys, sortKey{ID: id, Desc: desc})
	}
	return keys, nil
}

// fieldValueOf returns row's value for one of fieldIDsOf[T]()' column IDs (a
// string, bool, or int), or ok=false if id isn't a field of T.
func fieldValueOf[T any](row T, id string) (val interface{}, ok bool) {
	v := reflect.ValueOf(row)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if strings.Split(t.Field(i).Tag.Get("json"), ",")[0] == id {
			return v.Field(i).Interface(), true
		}
	}
	return nil, false
}

// compareFieldValues orders two fieldValueOf results of the same field
// (strings, bools — false < true —, or ints) — <0 if a<b, 0 if equal, >0 if
// a>b.
func compareFieldValues(a, b interface{}) int {
	switch av := a.(type) {
	case string:
		bv, _ := b.(string)
		switch {
		case av < bv:
			return -1
		case av > bv:
			return 1
		default:
			return 0
		}
	case bool:
		bv, _ := b.(bool)
		switch {
		case av == bv:
			return 0
		case !av && bv:
			return -1
		default:
			return 1
		}
	case int:
		bv, _ := b.(int)
		switch {
		case av < bv:
			return -1
		case av > bv:
			return 1
		default:
			return 0
		}
	default:
		return 0
	}
}

// sortRowsBy sorts rows in place by keys, in priority order (ties on one key
// are broken by the next), stably preserving the existing order for
// anything keys doesn't distinguish.
func sortRowsBy[T any](rows []T, keys []sortKey) {
	sort.SliceStable(rows, func(i, j int) bool {
		for _, key := range keys {
			vi, _ := fieldValueOf(rows[i], key.ID)
			vj, _ := fieldValueOf(rows[j], key.ID)
			c := compareFieldValues(vi, vj)
			if c == 0 {
				continue
			}
			if key.Desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
}

// buildFilteredRows uses reflect.StructOf to build, at runtime, a struct
// type containing only fieldIDs (in that order), copying each field's type
// and original tag from T — so JSON/TOON marshaling (which just reflects
// over whatever value it's given) emits exactly those fields, in that
// order, the same way fieldIDs narrows CSV's columns.
func buildFilteredRows[T any](rows []T, fieldIDs []string) interface{} {
	srcType := reflect.TypeOf((*T)(nil)).Elem()
	srcIndexByID := make(map[string]int, srcType.NumField())
	for i := 0; i < srcType.NumField(); i++ {
		id := strings.Split(srcType.Field(i).Tag.Get("json"), ",")[0]
		srcIndexByID[id] = i
	}

	structFields := make([]reflect.StructField, 0, len(fieldIDs))
	srcIndexes := make([]int, 0, len(fieldIDs))
	for _, id := range fieldIDs {
		idx, ok := srcIndexByID[id]
		if !ok {
			continue
		}
		srcField := srcType.Field(idx)
		structFields = append(structFields, reflect.StructField{
			Name: srcField.Name,
			Type: srcField.Type,
			Tag:  srcField.Tag,
		})
		srcIndexes = append(srcIndexes, idx)
	}

	dynType := reflect.StructOf(structFields)
	out := reflect.MakeSlice(reflect.SliceOf(dynType), len(rows), len(rows))
	for i, r := range rows {
		rv := reflect.ValueOf(r)
		dst := out.Index(i)
		for j, idx := range srcIndexes {
			dst.Field(j).Set(rv.Field(idx))
		}
	}
	return out.Interface()
}

// fieldCSVCell formats a fieldValueOf result for a CSV cell.
func fieldCSVCell(val interface{}) string {
	switch v := val.(type) {
	case bool:
		return fmt.Sprintf("%t", v)
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// flatRowsDoc is the generic JSON/TOON wrapper used whenever --fields or
// --sort-by switches a (normally region/org/space-nested) command's output
// to its flat, per-row view instead.
type flatRowsDoc struct {
	Rows interface{} `json:"rows" toon:"rows"`
}

// flatDocValue returns the value to marshal for JSON/TOON: the filtered
// (fieldIDs non-empty) or full flat row slice, wrapped as flatRowsDoc.
func flatDocValue[T any](rows []T, fieldIDs []string) interface{} {
	if len(fieldIDs) == 0 {
		return flatRowsDoc{Rows: rows}
	}
	return flatRowsDoc{Rows: buildFilteredRows(rows, fieldIDs)}
}

// writeFlatCSV writes rows as CSV, using fieldIDs as the header and column
// order (fieldIDsOf[T]() when fieldIDs is empty, i.e. every column).
func writeFlatCSV[T any](csvW csvWriter, rows []T, fieldIDs []string) error {
	ids := fieldIDs
	if len(ids) == 0 {
		ids = fieldIDsOf[T]()
	}
	if err := csvW.Write(ids); err != nil {
		return err
	}
	for _, r := range rows {
		cells := make([]string, len(ids))
		for i, id := range ids {
			val, _ := fieldValueOf(r, id)
			cells[i] = fieldCSVCell(val)
		}
		if err := csvW.Write(cells); err != nil {
			return err
		}
	}
	return nil
}

// csvWriter is the minimal interface writeFlatCSV needs — satisfied by both
// encoding/csv.Writer and this codebase's rainbowCSVWriter.
type csvWriter interface {
	Write(record []string) error
}
