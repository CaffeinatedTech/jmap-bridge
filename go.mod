module github.com/CaffeinatedTech/jmap-bridge

go 1.27.1

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/emersion/go-message v0.18.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kiliant/go-imap v1.1.0 // indirect
	github.com/kiliant/go-imap/imapserver v0.1.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.60.0 // indirect
)

// M4: the fork carries the X-GM-EXT-1 label store and the flag-form value
// capture the Gmail profile needs (~/projects/go-imap/PATCH-NOTES.md);
// revert to upstream when the PR merges.
replace github.com/kiliant/go-imap => ../go-imap
