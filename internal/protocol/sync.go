package protocol

import "encoding/json"

// TextDocumentSyncKind defines how the host (editor) should sync document changes.
type TextDocumentSyncKind = Integer

const (
	TextDocumentSyncKindNone        TextDocumentSyncKind = 0
	TextDocumentSyncKindFull        TextDocumentSyncKind = 1
	TextDocumentSyncKindIncremental TextDocumentSyncKind = 2
)

// TextDocumentSyncOptions describes text document sync capabilities.
type TextDocumentSyncOptions struct {
	OpenClose *bool                 `json:"openClose,omitempty"`
	Change    *TextDocumentSyncKind `json:"change,omitempty"`
}

// DidOpenTextDocumentParams are the parameters of a textDocument/didOpen notification.
type DidOpenTextDocumentParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// DidChangeTextDocumentParams are the parameters of a textDocument/didChange notification.
type DidChangeTextDocumentParams struct {
	TextDocument   VersionedTextDocumentIdentifier `json:"textDocument"`
	ContentChanges []any                           `json:"contentChanges"` // TextDocumentContentChangeEvent or TextDocumentContentChangeEventWhole
}

func (p *DidChangeTextDocumentParams) UnmarshalJSON(data []byte) error {
	var value struct {
		TextDocument   VersionedTextDocumentIdentifier `json:"textDocument"`
		ContentChanges []json.RawMessage               `json:"contentChanges"`
	}

	if err := json.Unmarshal(data, &value); err != nil {
		return err //nolint:wrapcheck // protocol marshaling
	}

	p.TextDocument = value.TextDocument

	for _, contentChange := range value.ContentChanges {
		var changeEvent TextDocumentContentChangeEvent
		if err := json.Unmarshal(contentChange, &changeEvent); err != nil {
			return err //nolint:wrapcheck // protocol marshaling
		}
		if changeEvent.Range != nil {
			p.ContentChanges = append(p.ContentChanges, changeEvent)
		} else {
			p.ContentChanges = append(p.ContentChanges, TextDocumentContentChangeEventWhole{
				Text: changeEvent.Text,
			})
		}
	}

	return nil
}

// TextDocumentContentChangeEvent describes a content change event with a range.
type TextDocumentContentChangeEvent struct {
	Range       *Range    `json:"range"`
	RangeLength *UInteger `json:"rangeLength,omitempty"`
	Text        string    `json:"text"`
}

// TextDocumentContentChangeEventWhole describes a full content replacement.
type TextDocumentContentChangeEventWhole struct {
	Text string `json:"text"`
}

// DidCloseTextDocumentParams are the parameters of a textDocument/didClose notification.
type DidCloseTextDocumentParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}
