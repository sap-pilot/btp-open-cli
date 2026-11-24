package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// ── --sort-nested-fields ─────────────────────────────────────────────────────

func TestNormalizeNestedFieldValue_PlainCommaList(t *testing.T) {
	got := normalizeNestedFieldValue("zebra,apple,mango")
	if got != "apple,mango,zebra" {
		t.Errorf("expected sorted comma list, got: %v", got)
	}
}

func TestNormalizeNestedFieldValue_PlainCommaList_Numeric(t *testing.T) {
	got := normalizeNestedFieldValue("10,2,1")
	if got != "1,2,10" {
		t.Errorf("expected numeric-aware sort (1,2,10), got: %v", got)
	}
}

func TestNormalizeNestedFieldValue_SemicolonDelimitedList(t *testing.T) {
	got := normalizeNestedFieldValue("zebra;apple;mango")
	if got != "apple;mango;zebra" {
		t.Errorf("expected sorted semicolon-delimited list rejoined with ';', got: %v", got)
	}
}

func TestNormalizeNestedFieldValue_CommaTakesPrecedenceOverSemicolon(t *testing.T) {
	// A value with both delimiters is split on ',' (the pre-existing
	// behavior), treating ';' as just part of each item's own text.
	got := normalizeNestedFieldValue("b;2,a;1")
	if got != "a;1,b;2" {
		t.Errorf("expected comma-delimited split with ';' left inside each item, got: %v", got)
	}
}

func TestNormalizeNestedFieldValue_JSONArrayString(t *testing.T) {
	got := normalizeNestedFieldValue(`["zebra","apple","mango"]`)
	if got != `["apple","mango","zebra"]` {
		t.Errorf("expected sorted JSON array string, got: %v", got)
	}
}

func TestNormalizeNestedFieldValue_NativeJSONArray(t *testing.T) {
	arr := []interface{}{"zebra", "apple", "mango"}
	got := normalizeNestedFieldValue(arr)
	want := []interface{}{"apple", "mango", "zebra"}
	gotArr, ok := got.([]interface{})
	if !ok || len(gotArr) != len(want) {
		t.Fatalf("expected a sorted native array, got: %v", got)
	}
	for i := range want {
		if gotArr[i] != want[i] {
			t.Errorf("index %d: expected %v, got %v", i, want[i], gotArr[i])
		}
	}
}

func TestNormalizeNestedFieldValue_JSONObjectString_KeysAlreadyCanonical(t *testing.T) {
	got := normalizeNestedFieldValue(`{"team":"payments","commit_id":"cd912038"}`)
	if got != `{"commit_id":"cd912038","team":"payments"}` {
		t.Errorf("expected object re-encoded with alphabetical keys, got: %v", got)
	}
}

func TestNormalizeNestedFieldValue_NestedArrayInsideObjectString(t *testing.T) {
	got := normalizeNestedFieldValue(`{"tags":["zebra","apple"]}`)
	if got != `{"tags":["apple","zebra"]}` {
		t.Errorf("expected the nested array sorted too, got: %v", got)
	}
}

func TestNormalizeNestedFieldValue_ArrayOfObjects(t *testing.T) {
	got := normalizeNestedFieldValue(`[{"name":"zebra"},{"name":"apple"}]`)
	if got != `[{"name":"apple"},{"name":"zebra"}]` {
		t.Errorf("expected array of objects sorted by their JSON form, got: %v", got)
	}
}

func TestNormalizeNestedFieldValue_ScalarUnchanged(t *testing.T) {
	for _, s := range []string{"42", "true", "null", `"quoted"`, "", "   "} {
		if got := normalizeNestedFieldValue(s); got != s {
			t.Errorf("expected scalar %q unchanged, got: %v", s, got)
		}
	}
}

func TestNormalizeNestedFieldValue_SingleItemNoComma(t *testing.T) {
	got := normalizeNestedFieldValue("solo-value")
	if got != "solo-value" {
		t.Errorf("expected single item unchanged, got: %v", got)
	}
}

func TestNormalizeNestedFieldValue_TrailingGarbageNotTreatedAsJSON(t *testing.T) {
	// Looks like it starts with valid JSON, but isn't valid JSON overall —
	// must not silently truncate to just the array part and lose "extra".
	// (It falls back to the plain comma-list rule instead, splitting on its
	// literal commas — not treated as JSON at all.)
	result, ok := normalizeNestedFieldValue("[1,2]extra").(string)
	if !ok {
		t.Fatal("expected a string result")
	}
	if result == "[1,2]" || !strings.Contains(result, "extra") {
		t.Errorf("expected trailing \"extra\" preserved (not silently dropped), got: %v", result)
	}
}

func TestParseWholeJSON_NumberPrecisionPreserved(t *testing.T) {
	v, ok := parseWholeJSON(`[1.50, 2]`)
	if !ok {
		t.Fatal("expected successful parse")
	}
	arr, ok := v.([]interface{})
	if !ok || len(arr) != 2 {
		t.Fatalf("expected a 2-element array, got: %v", v)
	}
	num, ok := arr[0].(json.Number)
	if !ok || num.String() != "1.50" {
		t.Errorf("expected json.Number \"1.50\" preserved exactly, got: %v", arr[0])
	}
}

// TestSort_NestedFieldsCSV is an end-to-end check of --sort-nested-fields
// against a CSV file with both a plain comma-list field and a JSON-encoded
// field holding an object with a nested array — reproducing the
// "don't let internal ordering show up as a diff" workflow this was added
// for.
func TestSort_NestedFieldsCSV(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "apps.csv")
	csvBody := "app_name,tags,app_labels\n" +
		`myapp,"zebra,apple,mango","{""tags"":[""z"",""a""],""team"":""payments""}"` + "\n"
	if err := os.WriteFile(in, []byte(csvBody), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sorted.csv")

	_, stderr, err := runCmd(t, "sort", "--sort-nested-fields=tags,app_labels", in, "-o", out, "-y")
	if err != nil {
		t.Fatalf("sort failed: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "apple,mango,zebra") {
		t.Errorf("expected the plain tags list sorted, got: %q", got)
	}
	if !strings.Contains(got, `""tags"":[""a"",""z""]`) {
		t.Errorf("expected the nested array inside app_labels sorted too, got: %q", got)
	}
}

// TestSort_NestedFieldsCSV_SemicolonDelimited verifies a tags-style column
// written with ';' instead of ',' (to sidestep the outer CSV's own comma)
// is sorted and rejoined with ';' too.
func TestSort_NestedFieldsCSV_SemicolonDelimited(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "apps.csv")
	csvBody := "app_name,tags\n" + "myapp,zebra;apple;mango\n"
	if err := os.WriteFile(in, []byte(csvBody), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sorted.csv")

	_, stderr, err := runCmd(t, "sort", "--sort-nested-fields=tags", in, "-o", out, "-y")
	if err != nil {
		t.Fatalf("sort failed: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "apple;mango;zebra") {
		t.Errorf("expected the semicolon-delimited tags list sorted, got: %q", got)
	}
}

// TestSort_NestedFieldsJSON_NativeArray is an end-to-end check against a
// JSON input file where the field holds a native JSON array rather than a
// JSON-encoded string.
func TestSort_NestedFieldsJSON_NativeArray(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "apps.json")
	jsonBody := `{"rows":[{"app_name":"myapp","tags":["zebra","apple","mango"]}]}`
	if err := os.WriteFile(in, []byte(jsonBody), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sorted.json")

	_, stderr, err := runCmd(t, "sort", "--sort-nested-fields=tags", in, "-o", out, "-y")
	if err != nil {
		t.Fatalf("sort failed: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `"apple",`) {
		t.Errorf("expected sorted tags array, got: %q", got)
	}
	idxApple := strings.Index(got, `"apple"`)
	idxZebra := strings.Index(got, `"zebra"`)
	if idxApple < 0 || idxZebra < 0 || idxApple > idxZebra {
		t.Errorf("expected apple before zebra in sorted output, got: %q", got)
	}
}

// TestSort_NestedFieldsAbsentFieldNoError verifies naming a field that
// doesn't exist on a row is a no-op, not an error.
func TestSort_NestedFieldsAbsentFieldNoError(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "apps.csv")
	csvBody := "app_name\nmyapp\n"
	if err := os.WriteFile(in, []byte(csvBody), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sorted.csv")

	_, stderr, err := runCmd(t, "sort", "--sort-nested-fields=nonexistent", in, "-o", out, "-y")
	if err != nil {
		t.Fatalf("sort should not error on an absent --sort-nested-fields field: %v\nstderr: %s", err, stderr)
	}
}

// ── --fields hash(...) ───────────────────────────────────────────────────────

func sha256Hex(t *testing.T, s string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestSortHashFieldInner(t *testing.T) {
	cases := []struct {
		field     string
		wantInner string
		wantOK    bool
	}{
		{"hash(password)", "password", true},
		{"hash(app_labels.commit_id)", "app_labels.commit_id", true},
		{"hash()", "", false},
		{"hash(password", "", false},
		{"hashpassword)", "", false},
		{"password", "", false},
		{"HASH(password)", "", false}, // case-sensitive, like every other field name here
	}
	for _, tc := range cases {
		inner, ok := sortHashFieldInner(tc.field)
		if ok != tc.wantOK || inner != tc.wantInner {
			t.Errorf("sortHashFieldInner(%q) = (%q, %v), want (%q, %v)", tc.field, inner, ok, tc.wantInner, tc.wantOK)
		}
	}
}

func TestSortHashValue(t *testing.T) {
	if got := sortHashValue("S3cr3t1"); got != sha256Hex(t, "S3cr3t1") {
		t.Errorf("expected SHA-256 hex digest, got %q", got)
	}
	if got := sortHashValue(""); got != "" {
		t.Errorf("expected empty string for an empty value, not SHA-256(''), got %q", got)
	}
	if got := sortHashValue(nil); got != "" {
		t.Errorf("expected empty string for a nil (absent) value, got %q", got)
	}
}

func TestResolveSortFieldOrHash(t *testing.T) {
	row := map[string]interface{}{"password": "S3cr3t1", "name": "API_HUB"}
	if got := resolveSortFieldOrHash(row, "hash(password)"); got != sha256Hex(t, "S3cr3t1") {
		t.Errorf("expected hashed password, got %v", got)
	}
	if got := resolveSortFieldOrHash(row, "name"); got != "API_HUB" {
		t.Errorf("expected a plain field to pass through unchanged, got %v", got)
	}
}

// TestSort_HashFieldCSV_SameValueSameHash is the exact scenario this
// feature exists for: two rows sharing the same secret value, compared
// without ever writing that value to disk.
func TestSort_HashFieldCSV_SameValueSameHash(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "destinations.csv")
	csvBody := "name,user,password,client_secret\n" +
		"API_HUB,api-user,S3cr3t1,\n" +
		"S4_BACKEND,s4-user,S3cr3t1,cs-xyz\n"
	if err := os.WriteFile(in, []byte(csvBody), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "destinations-sorted.csv")

	_, stderr, err := runCmd(t, "sort", "--fields", "name,user,hash(password),hash(client_secret)", in, "--output", out, "-y")
	if err != nil {
		t.Fatalf("sort failed: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)

	if strings.Contains(got, "S3cr3t1") || strings.Contains(got, "cs-xyz") {
		t.Fatalf("expected the real secret values never to appear in output, got: %q", got)
	}
	if !strings.Contains(got, "name,user,hash(password),hash(client_secret)") {
		t.Errorf("expected hash(...) columns literally named as given, got: %q", got)
	}
	pwHash := sha256Hex(t, "S3cr3t1")
	if strings.Count(got, pwHash) != 2 {
		t.Errorf("expected the same password hash on both rows (%s), got: %q", pwHash, got)
	}
	// client_secret differs: empty on one row (hashes to ""), set on the other.
	csHash := sha256Hex(t, "cs-xyz")
	if !strings.Contains(got, csHash) {
		t.Errorf("expected the client_secret hash for the row that has one, got: %q", got)
	}
	if !strings.Contains(got, "API_HUB,api-user,"+pwHash+",\n") {
		t.Errorf("expected an empty hash for the empty client_secret, got: %q", got)
	}
}

// TestSort_HashFieldJSON_AdditiveNotDestructive verifies hash(...) behaves
// like a dotted entry: additive, and the real field is dropped from output
// unless also named plainly.
func TestSort_HashFieldJSON_AdditiveNotDestructive(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "destinations.json")
	jsonBody := `{"rows":[{"name":"API_HUB","password":"S3cr3t1"}]}`
	if err := os.WriteFile(in, []byte(jsonBody), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sorted.json")

	_, stderr, err := runCmd(t, "sort", "--fields", "name,hash(password)", in, "-o", out, "-y")
	if err != nil {
		t.Fatalf("sort failed: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if strings.Contains(got, "S3cr3t1") {
		t.Fatalf("expected the real password value never to appear in output, got: %q", got)
	}
	if !strings.Contains(got, `"hash(password)": "`+sha256Hex(t, "S3cr3t1")+`"`) {
		t.Errorf("expected the hashed password field, got: %q", got)
	}
	if strings.Contains(got, `"password"`) {
		t.Errorf("expected the raw password field to be dropped (not named plainly in --fields), got: %q", got)
	}
}

// TestSort_HashFieldImplicitByGroupsIdenticalValues verifies that omitting
// --by (so the implicit sort is --fields' own order, including a
// hash(...) entry) actually sorts by the hash — not silently a no-op,
// since the hash column doesn't exist on the raw row until synthesized.
func TestSort_HashFieldImplicitByGroupsIdenticalValues(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "rows.csv")
	csvBody := "name,password\n" +
		"C_row,zzz\n" +
		"A_row,aaa\n" +
		"B_row,aaa\n"
	if err := os.WriteFile(in, []byte(csvBody), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sorted.csv")

	// No --by: the implicit sort is --fields' own order, i.e. by
	// hash(password) first.
	_, stderr, err := runCmd(t, "sort", "--fields", "name,hash(password)", in, "-o", out, "-y")
	if err != nil {
		t.Fatalf("sort failed: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected a header + 3 rows, got: %v", lines)
	}
	// aaa's hash groups A_row/B_row together (tied, so stable/name order
	// within the tie keeps A_row before B_row); zzz's hash sorts
	// differently and must not be interleaved between them.
	nameOf := func(line string) string { return strings.SplitN(line, ",", 2)[0] }
	aIdx, bIdx, cIdx := -1, -1, -1
	for i, l := range lines[1:] {
		switch nameOf(l) {
		case "A_row":
			aIdx = i
		case "B_row":
			bIdx = i
		case "C_row":
			cIdx = i
		}
	}
	if aIdx == -1 || bIdx == -1 || cIdx == -1 {
		t.Fatalf("expected all three rows present, got: %v", lines)
	}
	// C_row (a different hash) must land strictly outside the [aIdx, bIdx]
	// span — i.e. not interleaved between the two identical-hash rows.
	lo, hi := aIdx, bIdx
	if lo > hi {
		lo, hi = hi, lo
	}
	if cIdx > lo && cIdx < hi {
		t.Errorf("expected C_row (different hash) not interleaved between A_row/B_row (same hash), got order: %v", lines[1:])
	}
}
