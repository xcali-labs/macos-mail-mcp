package mail

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMailbox = "INBOX"
	maxSearchLimit = 200
)

// AppleScript payload format:
// - records are joined by ASCII RS (0x1e)
// - fields within each record are joined by ASCII US (0x1f)
// - list-valued fields are joined by ASCII GS (0x1d)

func quoteAS(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func quoteASMultiline(s string) string {
	normalized := strings.ReplaceAll(s, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	parts := strings.Split(normalized, "\n")
	if len(parts) == 0 {
		return quoteAS("")
	}
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		quoted = append(quoted, quoteAS(p))
	}
	return strings.Join(quoted, " & linefeed & ")
}

func asPrelude() string {
	return strings.TrimSpace(`
set fieldSep to (character id 31)
set recordSep to (character id 30)
set listSep to (character id 29)
`) + "\n"
}

// findMessageBlock locates one message by numeric id, using Mail's direct
// by-id reference: a "whose id is" query scans the mailbox and takes minutes in
// a large one. "message id" cannot be written because it parses as a property,
// so the raw class code is used. Runs inside a `tell
// application "Mail"` block. It expects messageIDText, messageIDNum,
// accountName and mailboxName to be set, and leaves the message in msg.
const findMessageBlock = `	set msg to missing value
	if accountName is "local" then
		repeat with mb in every mailbox
			try
				if (name of mb as text) is mailboxName then
					set matches to {}
					set cand to «class mssg» id messageIDNum of mb
					if (id of cand) is messageIDNum then set end of matches to cand
					if (count of matches) > 0 then
						set msg to item 1 of matches
						exit repeat
					end if
				end if
			end try
		end repeat
	end if
	if accountName is not "" then
		if exists account accountName then
			set accountList to {account accountName}
		else
			set accountList to {}
		end if
	else
		set accountList to every account
	end if

	repeat with acc in accountList
		if mailboxName is not "" then
			set candidateMailboxes to {}
			try
				set end of candidateMailboxes to (mailbox mailboxName of acc)
			end try
			if (count of candidateMailboxes) is 0 then
				repeat with mb in every mailbox of acc
					try
						if (name of mb as text) is mailboxName then
							set end of candidateMailboxes to mb
							exit repeat
						end if
					end try
				end repeat
			end if
			if (count of candidateMailboxes) is 0 then
				repeat with mb in every mailbox of acc
					try
						set mbName to (name of mb as text)
						ignoring case
							if (mbName contains mailboxName) or (mailboxName contains mbName) then
								set end of candidateMailboxes to mb
								exit repeat
							end if
						end ignoring
					end try
				end repeat
			end if
			repeat with mb in candidateMailboxes
				try
					set matches to {}
					set cand to «class mssg» id messageIDNum of mb
					if (id of cand) is messageIDNum then set end of matches to cand
				on error
					set matches to {}
				end try
				if (count of matches) > 0 then
					set msg to item 1 of matches
					exit repeat
				end if
			end repeat
		else
			try
				set inboxMatches to {}
				set cand to «class mssg» id messageIDNum of inbox of acc
				if (id of cand) is messageIDNum then set end of inboxMatches to cand
			on error
				set inboxMatches to {}
			end try
			if (count of inboxMatches) > 0 then
				set msg to item 1 of inboxMatches
			else
				repeat with mb in every mailbox of acc
					try
						set matches to {}
						set cand to «class mssg» id messageIDNum of mb
						if (id of cand) is messageIDNum then set end of matches to cand
					on error
						set matches to {}
					end try
					if (count of matches) > 0 then
						set msg to item 1 of matches
						exit repeat
					end if
				end repeat
			end if
		end if
		if msg is not missing value then
			exit repeat
		end if
	end repeat
	if msg is missing value then
		error "message_not_found:" & messageIDText
	end if
`

// asDateHandlers are top-level AppleScript handlers. stamp() returns a date as
// "Y-M-D-S" (S = seconds since local midnight) because AppleScript integers stop
// at 536870911, so seconds since 1970 cannot be coerced to an integer.
// mkDate() builds a local date from the same parts. Call both with "my".
const asDateHandlers = `on stamp(dt)
	if dt is missing value then return ""
	return ((year of dt) as text) & "-" & ((month of dt as integer) as text) & "-" & ((day of dt) as text) & "-" & ((time of dt) as text)
end stamp

on mkDate(y, m, d, s)
	set dt to current date
	set day of dt to 1
	set year of dt to y
	set month of dt to m
	set day of dt to d
	set time of dt to s
	return dt
end mkDate
`

// searchScanLimit is how many of the newest messages of a mailbox one search
// looks through by default; maxScanLimit is the most a caller may ask for.
const (
	searchScanLimit = 5000
	maxScanLimit    = 50000
	searchPageSize  = 250
	searchFirstPage = 60
	// searchTimeBudgetSeconds stops a search early (reported as truncated)
	// well before the osascript timeout.
	searchTimeBudgetSeconds = 45
)

func asDateLiteral(t *time.Time) string {
	if t == nil {
		return "missing value"
	}
	l := t.In(time.Local)
	secs := l.Hour()*3600 + l.Minute()*60 + l.Second()
	return fmt.Sprintf("my mkDate(%d, %d, %d, %d)", l.Year(), int(l.Month()), l.Day(), secs)
}

func buildListAccountsScript() string {
	return asPrelude() + strings.TrimSpace(`
tell application "Mail"
	set namesList to name of every account
end tell
if (count of namesList) is 0 then
	return ""
end if
set outText to ""
repeat with i from 1 to (count of namesList)
	if i > 1 then set outText to outText & recordSep
	set outText to outText & (item i of namesList as text)
end repeat
return outText
`)
}

func buildAccountIDsScript() string {
	return asPrelude() + strings.TrimSpace(`
tell application "Mail"
	set idList to id of every account
	set nameList to name of every account
end tell
set outText to ""
repeat with i from 1 to (count of idList)
	if i > 1 then set outText to outText & recordSep
	set outText to outText & (item i of idList as text) & fieldSep & (item i of nameList as text)
end repeat
return outText
`)
}

func buildOwnAddressesScript() string {
	return asPrelude() + strings.TrimSpace(`
set outText to ""
tell application "Mail"
	repeat with acc in every account
		repeat with a in (get email addresses of acc)
			if outText is not "" then set outText to outText & recordSep
			set outText to outText & (a as text)
		end repeat
	end repeat
end tell
return outText
`)
}

func buildListMailboxesScript(account string) string {
	return asPrelude() + fmt.Sprintf(strings.TrimSpace(`
set accountName to %s

tell application "Mail"
	if not (exists account accountName) then
		error "account_not_found:" & accountName
	end if
	set rows to {}
	repeat with mb in every mailbox of account accountName
		set row to (name of mb as text) & fieldSep & ((unread count of mb) as text)
		set end of rows to row
	end repeat
end tell
if (count of rows) is 0 then
	return ""
end if
set outText to ""
repeat with i from 1 to (count of rows)
	if i > 1 then set outText to outText & recordSep
	set outText to outText & (item i of rows as text)
end repeat
return outText
`), quoteAS(account))
}

func buildSearchMessagesScript(q SearchQuery) string {
	mailbox := q.Mailbox
	if mailbox == "" {
		mailbox = defaultMailbox
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	// Every filter runs in plain AppleScript over bulk-fetched property lists
	// of the newest messages. Mail's "whose" queries build the full match list
	// first and hang on large mailboxes; one bulk fetch per property is fast.
	scan := q.ScanLimit
	if scan <= 0 {
		scan = searchScanLimit
	}
	if scan > maxScanLimit {
		scan = maxScanLimit
	}
	unreadOnly := "false"
	if q.UnreadOnly {
		unreadOnly = "true"
	}

	return asPrelude() + asDateHandlers + fmt.Sprintf(strings.TrimSpace(`
set accountName to %s
set mailboxName to %s
set maxResults to %d
set scanLimit to %d
set pageSize to %d
set firstPageSize to %d
set timeBudget to %d
set senderNeedle to %s
set subjectNeedle to %s
set unreadOnly to %s
set dateFrom to %s
set dateTo to %s

tell application "Mail"
	if not (exists account accountName) then
		error "account_not_found:" & accountName
	end if
	set acc to account accountName
	set targetMailbox to missing value
	ignoring case
		if mailboxName is "inbox" then
			try
				set targetMailbox to inbox of acc
			end try
		end if
	end ignoring
	if targetMailbox is missing value then
		try
			set targetMailbox to mailbox mailboxName of acc
		end try
	end if
	if targetMailbox is missing value then
		repeat with mb in every mailbox of acc
			try
				set mbName to (name of mb as text)
				ignoring case
					if (mbName is mailboxName) or (mbName contains mailboxName) or (mailboxName contains mbName) then
						set targetMailbox to mb
						exit repeat
					end if
				end ignoring
			end try
		end repeat
	end if
	if targetMailbox is missing value then
		error "mailbox_not_found:" & mailboxName
	end if
	set mailboxSize to count of messages of targetMailbox
end tell
set n to mailboxSize
if n > scanLimit then set n to scanLimit
set startTime to current date
set truncated to false
set scanned to 0

set rows to {}
set collectedCount to 0
set pageStart to 1
set thisPage to firstPageSize
repeat while pageStart <= n and collectedCount < maxResults
	if ((current date) - startTime) > timeBudget then
		set truncated to true
		exit repeat
	end if
	set pageEnd to pageStart + thisPage - 1
	if pageEnd > n then set pageEnd to n
	set pageCount to pageEnd - pageStart + 1
	-- Fetch only the properties a filter needs (each read costs Mail ~8 ms per
	-- message); the rest is read later for matching messages only.
	tell application "Mail"
		if unreadOnly then set readList to read status of messages pageStart thru pageEnd of targetMailbox
		if senderNeedle is not "" then set senderList to sender of messages pageStart thru pageEnd of targetMailbox
		if subjectNeedle is not "" then set subjList to subject of messages pageStart thru pageEnd of targetMailbox
		if (dateFrom is not missing value) or (dateTo is not missing value) then set dateList to date received of messages pageStart thru pageEnd of targetMailbox
	end tell
	set matchIdx to {}
	repeat with i from 1 to pageCount
		if (count of matchIdx) + collectedCount >= maxResults then exit repeat
		set keep to true
		try
			if unreadOnly then
				if item i of readList then set keep to false
			end if
			if keep and senderNeedle is not "" then
				if ((item i of senderList) as text) does not contain senderNeedle then set keep to false
			end if
			if keep and subjectNeedle is not "" then
				if ((item i of subjList) as text) does not contain subjectNeedle then set keep to false
			end if
			if keep and ((dateFrom is not missing value) or (dateTo is not missing value)) then
				set d to item i of dateList
				if d is missing value then
					set keep to false
				else
					if dateFrom is not missing value then
						if d < dateFrom then set keep to false
					end if
					if dateTo is not missing value then
						if d > dateTo then set keep to false
					end if
				end if
			end if
		on error
			set keep to false
		end try
		if keep then set end of matchIdx to (pageStart + i - 1)
	end repeat
	tell application "Mail"
		repeat with k in matchIdx
			try
				set msg to message (k as integer) of targetMailbox
				set msgDate to missing value
				try
					set msgDate to date received of msg
				end try
				set row to (id of msg as text) & fieldSep & (subject of msg as text) & fieldSep & (sender of msg as text) & fieldSep & (my stamp(msgDate)) & fieldSep & ((read status of msg) as text) & fieldSep & mailboxName
				set end of rows to row
				set collectedCount to collectedCount + 1
			end try
		end repeat
	end tell
	set scanned to pageEnd
	set pageStart to pageEnd + 1
	set thisPage to pageSize
end repeat
set outText to "META" & fieldSep & (scanned as text) & fieldSep & (mailboxSize as text) & fieldSep & (truncated as text)
repeat with i from 1 to (count of rows)
	set outText to outText & recordSep & (item i of rows as text)
end repeat
return outText
`), quoteAS(q.Account), quoteAS(mailbox), limit, scan, searchPageSize, searchFirstPage, searchTimeBudgetSeconds, quoteAS(q.SenderContains), quoteAS(q.SubjectContains), unreadOnly, asDateLiteral(q.DateFrom), asDateLiteral(q.DateTo))
}

func buildGetMessageScript(messageID, account, mailbox string, includeBody bool) string {
	includeBodyValue := "false"
	if includeBody {
		includeBodyValue = "true"
	}
	return asPrelude() + asDateHandlers + fmt.Sprintf(strings.TrimSpace(`
set messageIDText to %s
set messageIDNum to (messageIDText as integer)
set includeBody to %s
set accountName to %s
set mailboxName to %s

tell application "Mail"
`+findMessageBlock+`	set recipList to address of every to recipient of msg
	set ccList to address of every cc recipient of msg
	set msgID to (id of msg as text)
	set msgSubject to (subject of msg as text)
	set msgSender to (sender of msg as text)
	set msgDateValue to missing value
	try
		set msgDateValue to date received of msg
	end try
	if msgDateValue is missing value then
		try
			set msgDateValue to date sent of msg
		end try
	end if
	set msgUnixDate to my stamp(msgDateValue)
	set msgMailbox to (name of mailbox of msg as text)
	set msgRead to ((read status of msg) as text)
	set msgFlagged to ((flagged status of msg) as text)
	set msgBody to ""
	if includeBody then
		try
			set msgBody to (content of msg as text)
		on error
			set msgBody to ""
		end try
	end if
end tell
if (count of recipList) is 0 then
	set recips to ""
else
	set recips to ""
	repeat with i from 1 to (count of recipList)
		if i > 1 then set recips to recips & listSep
		set recips to recips & (item i of recipList as text)
	end repeat
end if
if (count of ccList) is 0 then
	set ccRecips to ""
else
	set ccRecips to ""
	repeat with i from 1 to (count of ccList)
		if i > 1 then set ccRecips to ccRecips & listSep
		set ccRecips to ccRecips & (item i of ccList as text)
	end repeat
end if
set row to msgID & fieldSep & msgSubject & fieldSep & msgSender & fieldSep & recips & fieldSep & ccRecips & fieldSep & msgUnixDate & fieldSep & msgBody & fieldSep & msgMailbox & fieldSep & msgRead & fieldSep & msgFlagged
return row
`), quoteAS(messageID), includeBodyValue, quoteAS(account), quoteAS(mailbox))
}

// buildDraftScript creates a new outgoing message and saves it unsent in
// Drafts. This fork has no send path: the person presses Send in Mail.
func buildDraftScript(input ComposeInput) string {
	toList := makeAppleScriptAddressLoop("to recipients", input.To)
	ccList := makeAppleScriptAddressLoop("cc recipients", input.CC)

	// Outgoing messages have no account property: the account is chosen by
	// setting the sender to that account's first address.
	accountSnippet := ""
	if input.Account != "" {
		accountSnippet = fmt.Sprintf(`
	if not (exists account %s) then
		error "account_not_found:" & %s
	end if
	set senderAddress to item 1 of (get email addresses of account %s)
`, quoteAS(input.Account), quoteAS(input.Account), quoteAS(input.Account))
	}
	senderProp := ""
	if input.Account != "" {
		senderProp = ", sender:senderAddress"
	}

	return asPrelude() + fmt.Sprintf(strings.TrimSpace(`
set subjectText to %s
set contentText to %s

tell application "Mail"%s
	set newMessage to make new outgoing message with properties {visible:false, subject:subjectText, content:contentText%s}
	tell newMessage
%s
%s
	end tell
	save newMessage
	return (id of newMessage as text)
end tell
`), quoteAS(input.Subject), quoteASMultiline(input.Body), accountSnippet, senderProp, toList, ccList)
}

// buildReplyTargetScript reads who a message came from and its Reply-To,
// changing nothing, so a reply to a different address can be refused first.
func buildReplyTargetScript(messageID, account, mailbox string) string {
	return asPrelude() + fmt.Sprintf(strings.TrimSpace(`
set messageIDText to %s
set messageIDNum to (messageIDText as integer)
set accountName to %s
set mailboxName to %s

tell application "Mail"
`+findMessageBlock+`	set senderAddress to ""
	try
		set senderAddress to (extract address from (sender of msg))
	end try
	set replyAddress to ""
	try
		set replyAddress to (extract address from (reply to of msg))
	end try
	return senderAddress & fieldSep & replyAddress
end tell
`), quoteAS(messageID), quoteAS(account), quoteAS(mailbox))
}

// buildReplyDraftScript makes Mail's own reply to a message (so it threads,
// with Re: subject and reply headers), puts the new text above a quoted copy
// of the original, saves it unsent in Drafts, and returns where it is addressed.
// It never replies to all.
func buildReplyDraftScript(messageID, account, mailbox, body string) string {
	return asPrelude() + fmt.Sprintf(strings.TrimSpace(`
set messageIDText to %s
set messageIDNum to (messageIDText as integer)
set accountName to %s
set mailboxName to %s
set replyText to %s

tell application "Mail"
`+findMessageBlock+`	set origContent to ""
	try
		set origContent to (content of msg as text)
	end try
	set origSender to ""
	try
		set origSender to (sender of msg as text)
	end try
	set origDate to ""
	try
		set origDate to ((date received of msg) as text)
	end try
	set quotedText to ""
	repeat with p in (paragraphs of origContent)
		set quotedText to quotedText & "> " & (p as text) & linefeed
	end repeat
	set replyMsg to reply msg opening window false reply to all false
	set content of replyMsg to replyText & linefeed & linefeed & origDate & ", " & origSender & ":" & linefeed & quotedText
	save replyMsg
	set toList to address of every to recipient of replyMsg
	set ccList to address of every cc recipient of replyMsg
end tell
set outText to ""
repeat with a in toList
	if outText is not "" then set outText to outText & listSep
	set outText to outText & (a as text)
end repeat
repeat with a in ccList
	if outText is not "" then set outText to outText & listSep
	set outText to outText & (a as text)
end repeat
return outText
`), quoteAS(messageID), quoteAS(account), quoteAS(mailbox), quoteASMultiline(body))
}

// buildMoveMessageScript moves one message to another mailbox of the same
// account. The target must match a mailbox name exactly: no fuzzy matching.
func buildMoveMessageScript(messageID, account, mailbox, target string) string {
	return asPrelude() + fmt.Sprintf(strings.TrimSpace(`
set messageIDText to %s
set messageIDNum to (messageIDText as integer)
set accountName to %s
set mailboxName to %s
set targetName to %s

tell application "Mail"
`+findMessageBlock+`	if accountName is "local" then
		set candidateBoxes to every mailbox
	else
		set candidateBoxes to every mailbox of (account of (mailbox of msg))
	end if
	set targetMailbox to missing value
	repeat with mb in candidateBoxes
		if (name of mb as text) is targetName then
			set targetMailbox to mb
			exit repeat
		end if
	end repeat
	if targetMailbox is missing value then
		error "target_mailbox_not_found:" & targetName
	end if
	move msg to targetMailbox
	return "moved"
end tell
`), quoteAS(messageID), quoteAS(account), quoteAS(mailbox), quoteAS(target))
}

func buildUnreadCountScript(account string) string {
	accountFilter := ""
	if account != "" {
		accountFilter = fmt.Sprintf(`
	if not (exists account %s) then
		error "account_not_found:" & %s
	end if
	set accountList to {account %s}
`, quoteAS(account), quoteAS(account), quoteAS(account))
	}

	if accountFilter == "" {
		accountFilter = "\n\tset accountList to every account\n"
	}

	return asPrelude() + strings.TrimSpace(fmt.Sprintf(`
set rows to {}
set totalUnread to 0

tell application "Mail"%s
	repeat with acc in accountList
		repeat with mb in every mailbox of acc
			set unreadCount to unread count of mb
			set totalUnread to totalUnread + unreadCount
			set row to (name of acc as text) & fieldSep & (name of mb as text) & fieldSep & (unreadCount as text)
			set end of rows to row
		end repeat
	end repeat
end tell
if (count of rows) is 0 then
	return totalUnread as text
end if
set rowsText to ""
repeat with i from 1 to (count of rows)
	if i > 1 then set rowsText to rowsText & recordSep
	set rowsText to rowsText & (item i of rows as text)
end repeat
return (totalUnread as text) & recordSep & rowsText
`, accountFilter))
}

func makeAppleScriptAddressLoop(recipientKind string, addrs []string) string {
	if len(addrs) == 0 {
		return ""
	}
	var b strings.Builder
	for i, addr := range addrs {
		name := "a" + strconv.Itoa(i)
		b.WriteString("\n\t\tset ")
		b.WriteString(name)
		b.WriteString(" to ")
		b.WriteString(quoteAS(addr))
		b.WriteString("\n\t\tmake new ")
		b.WriteString(strings.TrimSuffix(recipientKind, "s"))
		b.WriteString(" at end of ")
		b.WriteString(recipientKind)
		b.WriteString(" with properties {address:")
		b.WriteString(name)
		b.WriteString("}")
	}
	return b.String()
}
