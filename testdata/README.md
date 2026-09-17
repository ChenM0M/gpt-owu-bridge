# Test fixtures

`synthetic-share.html` is entirely fictional. It models the one observed
React Router reference-table shape without containing a real share URL,
account identifier, cookie, token, or conversation.

The visible path deliberately contains Chinese text, a Go code block,
newlines, inline math, display math, an assistant commentary message, and an
internal tool node. Parser tests verify that supported user-facing text is
preserved byte-for-byte while the tool node is excluded and counted.

This fixture proves only the local M1 parser contract. It is not evidence that
the current ChatGPT share site still emits this exact shape or that Open WebUI
accepts the resulting snapshot.
