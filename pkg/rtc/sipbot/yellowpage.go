package sipbot

// This file is the bot's yellow page (docs/sip-bot.md §10): the
// deployment's phone book of example callable numbers — the
// <yellowPage/> element of serverConfig.xml — printed into the chat by
// the CLI's /yellow-page command, so a user can pick a callee without
// memorizing numbers. The page is plain static data: the wiring hands
// it to the handler verbatim, and nothing here is validated — a
// contact's aor above all, which is anything /call accepts; a bad one
// simply fails when someone /calls it, with /call's own error for an
// answer.

import (
	"fmt"
	"strings"
)

// YellowPageSection is one section of the yellow page: a named group of
// contacts. ID is an opaque string uniquely identifying the section in
// the configuration document; Name is the display caption the
// /yellow-page listing prints.
type YellowPageSection struct {
	ID       string
	Name     string
	Contacts []YellowPageContact
}

// YellowPageContact is one example callable number of a yellow-page
// section. ID is an opaque string uniquely identifying the contact in
// the configuration document; Name the display name; Description an
// optional one-line note; AOR the dial target — anything the CLI's
// /call accepts (a bare user "9196", user@host, or a full SIP URI).
type YellowPageContact struct {
	ID          string
	Name        string
	Description string
	AOR         string
}

// yellowPageEmpty is the /yellow-page answer when the bot's page carries
// no sections.
const yellowPageEmpty = "The yellow page is empty."

// yellowPageText renders the /yellow-page answer: the page's sections in
// order, each a caption line of its own, each contact a line beneath —
// the name, then the aor the user can hand straight to /call, then the
// description when the entry carries one.
func yellowPageText(sections []YellowPageSection) string {
	if len(sections) == 0 {
		return yellowPageEmpty
	}
	var b strings.Builder
	b.WriteString("Yellow page — dial with /call <aor>:")
	for _, section := range sections {
		fmt.Fprintf(&b, "\n%s:", section.Name)
		for _, contact := range section.Contacts {
			fmt.Fprintf(&b, "\n- %s: %s", contact.Name, contact.AOR)
			if contact.Description != "" {
				fmt.Fprintf(&b, " — %s", contact.Description)
			}
		}
	}
	return b.String()
}
