package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSortFieldValue_PlainField(t *testing.T) {
	row := map[string]interface{}{"space_name": "dev"}
	if got := resolveSortFieldValue(row, "space_name"); got != "dev" {
		t.Errorf("expected %q, got %v", "dev", got)
	}
	if got := resolveSortFieldValue(row, "missing"); got != nil {
		t.Errorf("expected nil for a missing plain field, got %v", got)
	}
}

func TestResolveSortFieldValue_DottedIntoNativeMap(t *testing.T) {
	row := map[string]interface{}{
		"app_labels": map[string]interface{}{"commit_id": "cd912038", "team": "payments"},
	}
	if got := resolveSortFieldValue(row, "app_labels.commit_id"); got != "cd912038" {
		t.Errorf("expected commit_id value, got %v", got)
	}
}

func TestResolveSortFieldValue_DottedIntoJSONString(t *testing.T) {
	// The CSV shape: every value, including a nested object, arrives as a
	// plain JSON-encoded string (e.g. from 'bo apps --include-labels --format csv').
	row := map[string]interface{}{
		"app_labels": `{"commit_id":"cd912038","team":"payments"}`,
	}
	if got := resolveSortFieldValue(row, "app_labels.commit_id"); got != "cd912038" {
		t.Errorf("expected commit_id value decoded from the JSON string, got %v", got)
	}
}

func TestResolveSortFieldValue_MissingSubKeyInValidObject(t *testing.T) {
	row := map[string]interface{}{
		"app_labels": map[string]interface{}{"team": "payments"},
	}
	if got := resolveSortFieldValue(row, "app_labels.commit_id"); got != nil {
		t.Errorf("expected nil for a key missing from an otherwise-valid object, got %v", got)
	}
}

func TestResolveSortFieldValue_FallsBackToWholeStringWhenNotAnObject(t *testing.T) {
	cases := []struct {
		name string
		val  interface{}
	}{
		{"plain non-JSON string", "plainstring"},
		{"empty string", ""},
		{"JSON scalar string", `"just a string"`},
		{"JSON array string", `["a","b"]`},
	}
	for _, tc := range cases {
		row := map[string]interface{}{"app_labels": tc.val}
		got := resolveSortFieldValue(row, "app_labels.commit_id")
		if got != tc.val {
			t.Errorf("%s: expected fallback to the whole parent value %v, got %v", tc.name, tc.val, got)
		}
	}
}

func TestResolveSortFieldValue_MissingParentField(t *testing.T) {
	row := map[string]interface{}{"other": "x"}
	if got := resolveSortFieldValue(row, "app_labels.commit_id"); got != nil {
		t.Errorf("expected nil when the parent field itself is absent, got %v", got)
	}
}

func TestResolveSortFieldValue_MultiLevelDotted(t *testing.T) {
	row := map[string]interface{}{
		"a": map[string]interface{}{
			"b": map[string]interface{}{"c": "deep-value"},
		},
	}
	if got := resolveSortFieldValue(row, "a.b.c"); got != "deep-value" {
		t.Errorf("expected deep-value, got %v", got)
	}
}

// TestSort_DottedFieldsCSV is an end-to-end check of --fields with a dotted
// field against a CSV file shaped like 'bo apps --include-labels” own
// output, reproducing the workflow this feature was added for.
func TestSort_DottedFieldsCSV(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "apps.csv")
	csvBody := "space_name,app_name,app_labels\n" +
		`dev,myapp-srv,"{""commit_id"":""cd912038"",""team"":""payments""}"` + "\n" +
		`prod,myapp-ui,` + "\n"
	if err := os.WriteFile(in, []byte(csvBody), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sorted.csv")

	_, stderr, err := runCmd(t, "sort", "--fields=space_name,app_name,app_labels.commit_id", in, "-o", out, "-y")
	if err != nil {
		t.Fatalf("sort failed: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "space_name,app_name,app_labels.commit_id") {
		t.Errorf("expected dotted field as its own CSV column header, got: %q", got)
	}
	if !strings.Contains(got, "dev,myapp-srv,cd912038") {
		t.Errorf("expected extracted commit_id value for dev row, got: %q", got)
	}
	if !strings.Contains(got, "prod,myapp-ui,\n") {
		t.Errorf("expected an empty commit_id cell for the row with no app_labels, got: %q", got)
	}
}

// TestSort_DottedByCSV confirms --by with a dotted field sorts using the
// extracted sub-value, not the parent's raw (JSON-encoded) string.
func TestSort_DottedByCSV(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "apps.csv")
	csvBody := "app_name,app_labels\n" +
		`b-app,"{""commit_id"":""2""}"` + "\n" +
		`a-app,"{""commit_id"":""10""}"` + "\n"
	if err := os.WriteFile(in, []byte(csvBody), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sorted.csv")

	_, stderr, err := runCmd(t, "sort", "--fields=app_name,app_labels", "--by=app_labels.commit_id", in, "-o", out, "-y")
	if err != nil {
		t.Fatalf("sort failed: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "b-app,") || !strings.HasPrefix(lines[2], "a-app,") {
		t.Errorf("expected numeric sort by commit_id (2 before 10), got: %v", lines)
	}
}

// TestSort_DottedFieldsJSON_AdditiveNotDestructive confirms a dotted
// --fields entry adds its own synthetic key without dropping the (always-
// kept, non-leaf) parent field it was extracted from.
func TestSort_DottedFieldsJSON_AdditiveNotDestructive(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "apps.json")
	jsonBody := `{"rows":[{"app_name":"myapp-srv","app_labels":{"commit_id":"cd912038","team":"payments"}}]}`
	if err := os.WriteFile(in, []byte(jsonBody), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sorted.json")

	_, stderr, err := runCmd(t, "sort", "--fields=app_name,app_labels.commit_id", in, "-o", out, "-y")
	if err != nil {
		t.Fatalf("sort failed: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `"app_labels.commit_id": "cd912038"`) {
		t.Errorf("expected the dotted field's extracted value, got: %q", got)
	}
	if !strings.Contains(got, `"team": "payments"`) {
		t.Errorf("expected the parent app_labels object to still be present whole, got: %q", got)
	}
}
