package simperium

import (
	"encoding/json"
	"testing"
)

func TestNoteUnmarshalLenient(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Note
	}{
		{
			name: "normal booleans and numbers",
			raw:  `{"content":"hi","deleted":true,"creationDate":1,"modificationDate":2,"tags":["a"],"systemTags":["pinned"]}`,
			want: Note{Content: "hi", Deleted: true, CreationDate: 1, ModificationDate: 2, Tags: []string{"a"}, SystemTags: []string{"pinned"}},
		},
		{
			name: "numeric deleted",
			raw:  `{"content":"hi","deleted":1,"creationDate":1,"modificationDate":2}`,
			want: Note{Content: "hi", Deleted: true, CreationDate: 1, ModificationDate: 2},
		},
		{
			name: "zero deleted",
			raw:  `{"content":"hi","deleted":0}`,
			want: Note{Content: "hi", Deleted: false},
		},
		{
			name: "string dates and null content",
			raw:  `{"content":null,"deleted":false,"creationDate":"12.5","modificationDate":"99.25"}`,
			want: Note{Content: "", Deleted: false, CreationDate: 12.5, ModificationDate: 99.25},
		},
		{
			name: "missing fields",
			raw:  `{}`,
			want: Note{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Note
			if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(got.Tags) == 0 {
				got.Tags = nil
			}
			if got.Content != tc.want.Content || got.Deleted != tc.want.Deleted ||
				got.CreationDate != tc.want.CreationDate || got.ModificationDate != tc.want.ModificationDate {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestNoteMarshalStaysBoolean(t *testing.T) {
	b, err := json.Marshal(Note{Content: "x", Deleted: true})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["deleted"] != true {
		t.Fatalf("deleted should marshal as a JSON boolean, got %T %v", m["deleted"], m["deleted"])
	}
}
