Enhancement: Enable EnableRemoteLinkPicker WOPI flag for Collabora

Set `EnableRemoteLinkPicker: true` in the CheckFileInfo response so that
Collabora Online exposes the "Insert Link" UI backed by the WOPI host. When
the user picks this option, Collabora sends a `UI_PickLink` postMessage that
the WOPI host is expected to answer with `Action_InsertLink` carrying the URL
of the selected file.

https://github.com/opencloud-eu/opencloud/pull/2610
