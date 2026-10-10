package imapd

import (
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/mirror"
)

// SEARCH is answered from the mirror only, and only where the mirror gives
// the exact answer: sequence numbers, uids, flags, internal date and size,
// combined with NOT, OR and AND. Criteria that need header or content are
// refused; a partial result is never passed off as complete.

var (
	errSearchContent = &imap.Error{
		Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot,
		Text: "Search in headers or text is not available on this server",
	}
	errSearchComplex = &imap.Error{
		Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit,
		Text: "Search is too complex",
	}
	errSearchRes = &imap.Error{
		Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot,
		Text: "Saved search results are not supported",
	}
	errTooManyRanges = &imap.Error{
		Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit,
		Text: "Too many ranges in one command",
	}
)

// toRanges converts a number set of the library. "*" becomes 0.
func toRanges(set imap.NumSet, max int) ([]mirror.Range, bool, error) {
	if imap.IsSearchRes(set) {
		return nil, false, errSearchRes
	}
	switch s := set.(type) {
	case imap.SeqSet:
		if len(s) > max {
			return nil, false, errTooManyRanges
		}
		out := make([]mirror.Range, len(s))
		for i, r := range s {
			out[i] = mirror.Range{Lo: r.Start, Hi: r.Stop}
		}
		return out, false, nil
	case imap.UIDSet:
		if len(s) > max {
			return nil, true, errTooManyRanges
		}
		out := make([]mirror.Range, len(s))
		for i, r := range s {
			out[i] = mirror.Range{Lo: uint32(r.Start), Hi: uint32(r.Stop)}
		}
		return out, true, nil
	}
	return nil, false, errTooManyRanges
}

// crit is a validated search tree with its number sets resolved.
type crit struct {
	seq      [][]mirror.Range
	uid      [][]mirror.Range
	since    time.Time
	before   time.Time
	flags    []imap.Flag
	notFlags []imap.Flag
	larger   int64
	smaller  int64
	not      []crit
	or       [][2]crit
}

type searchBudget struct {
	maxDepth  int
	nodes     int
	maxRanges int
}

func uniqueFlags(in []imap.Flag) []imap.Flag {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]imap.Flag, 0, len(in))
	for _, f := range in {
		k := strings.ToLower(string(f))
		if !seen[k] {
			seen[k] = true
			out = append(out, f)
		}
	}
	return out
}

// compile validates c and resolves "*" against the client's view. Every
// single criterion counts against the budget, not only every level: a flat
// list of a thousand flags costs as much to evaluate as a deep tree.
func compile(c *imap.SearchCriteria, depth int, b *searchBudget, maxSeq, maxUID uint32) (*crit, error) {
	b.nodes -= 1 + len(c.Flag) + len(c.NotFlag) + len(c.SeqNum) + len(c.UID)
	if depth > b.maxDepth || b.nodes < 0 {
		return nil, errSearchComplex
	}
	if len(c.Header) > 0 || len(c.Body) > 0 || len(c.Text) > 0 ||
		!c.SentSince.IsZero() || !c.SentBefore.IsZero() || c.ModSeq != nil {
		return nil, errSearchContent
	}
	out := &crit{
		since: c.Since, before: c.Before,
		flags: uniqueFlags(c.Flag), notFlags: uniqueFlags(c.NotFlag),
		larger: c.Larger, smaller: c.Smaller,
	}
	for _, s := range c.SeqNum {
		r, _, err := toRanges(s, b.maxRanges)
		if err != nil {
			return nil, err
		}
		out.seq = append(out.seq, mirror.Normalize(r, maxSeq))
	}
	for _, s := range c.UID {
		r, _, err := toRanges(s, b.maxRanges)
		if err != nil {
			return nil, err
		}
		out.uid = append(out.uid, mirror.Normalize(r, maxUID))
	}
	for i := range c.Not {
		n, err := compile(&c.Not[i], depth+1, b, maxSeq, maxUID)
		if err != nil {
			return nil, err
		}
		out.not = append(out.not, *n)
	}
	for i := range c.Or {
		l, err := compile(&c.Or[i][0], depth+1, b, maxSeq, maxUID)
		if err != nil {
			return nil, err
		}
		r, err := compile(&c.Or[i][1], depth+1, b, maxSeq, maxUID)
		if err != nil {
			return nil, err
		}
		out.or = append(out.or, [2]crit{*l, *r})
	}
	return out, nil
}

func (c *crit) match(it *mirror.Item) bool {
	for _, s := range c.seq {
		if !mirror.InRanges(s, it.Seq) {
			return false
		}
	}
	for _, s := range c.uid {
		if !mirror.InRanges(s, it.Msg.UID) {
			return false
		}
	}
	if !c.since.IsZero() || !c.before.IsZero() {
		// INTERNALDATE is reported in UTC, so its date is the UTC date.
		t := time.UnixMilli(it.Msg.Date).UTC()
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		if !c.since.IsZero() && day.Before(c.since) {
			return false
		}
		if !c.before.IsZero() && !day.Before(c.before) {
			return false
		}
	}
	for _, f := range c.flags {
		if !mirror.HasFlag(it.Msg.Flags, f) {
			return false
		}
	}
	for _, f := range c.notFlags {
		if mirror.HasFlag(it.Msg.Flags, f) {
			return false
		}
	}
	if c.larger != 0 && it.Msg.Size <= c.larger {
		return false
	}
	if c.smaller != 0 && it.Msg.Size >= c.smaller {
		return false
	}
	for i := range c.not {
		if c.not[i].match(it) {
			return false
		}
	}
	for i := range c.or {
		if !c.or[i][0].match(it) && !c.or[i][1].match(it) {
			return false
		}
	}
	return true
}
