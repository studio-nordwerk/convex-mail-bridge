package main

import (
	"bytes"
	"fmt"
	"math"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
)

// The mailboxes of the dev stack are filled with invented mail of a small
// workshop: no real person, company or address, every domain ends in .test.
// The first mailbox is the large one (for trying a real mail program), the
// others are small. Names, emoji and dashes in subjects are deliberate
// non-ASCII data for the header encoding.

type person struct{ name, address string }

func (p person) String() string {
	return (&mail.Address{Name: p.name, Address: p.address}).String()
}

var people = []person{
	{"Holzhandel Nordlicht", "bestellung@holzhandel-nordlicht.test"},
	{"Jörg Möllenkamp", "joerg@moellenkamp-ausbau.test"},
	{"Zoë Çakır", "zoe.cakir@kueche-und-raum.test"},
	{"Björn Åkesson 🚲", "bjoern@radwerk-fjord.test"},
	{"Tischlerei Großebüscher & Söhne", "kontakt@grossebuescher.test"},
	{"Café Seeblick ☕", "hallo@cafe-seeblick.test"},
	{"Hofladen Ünal 🌻", "post@hofladen-uenal.test"},
	{"Dr. Renée Weißenfels", "r.weissenfels@praxis-am-deich.test"},
	{"Łukasz Wiśniewski", "lukasz@wisniewski-montage.test"},
	{"Kita Löwenzahn", "leitung@kita-loewenzahn.test"},
	{"Steuerbüro Rehfeld", "rechnung@steuerbuero-rehfeld.test"},
	{"Marlène Dufour", "marlene@atelier-dufour.test"},
	{"Sæmundur Jónsson", "saemundur@nordlys-bau.test"},
	{"Paketdienst Flinkfuß 📦", "sendung@flinkfuss.test"},
	{"Werkzeugverleih Hafenkante", "service@hafenkante-verleih.test"},
	{"Familie Östergård", "oestergard@privatpost.test"},
}

var topics = []struct{ subject, text string }{
	{"Quote for the kitchen 🍳", "Hi,\n\nattached is the quote for the kitchen run: oiled oak, handleless fronts.\nLead time about three weeks.\n\nBest regards"},
	{"Question: door frame dimensions", "Hello,\n\nthe frame is 2.01 m high and 88.5 cm wide. Does that still fit your design?\nThe sockets stay where they are.\n\nThanks and regards"},
	{"Invoice 2026-0931", "Good day,\n\ninvoice 2026-0931 for €1,284.50 is attached.\nPayable within 14 days.\n\nKind regards"},
	{"Delivery slips to week 42", "Hi,\n\nthe fittings will unfortunately only arrive in week 42.\nWe will get in touch as soon as the shipment is on its way."},
	{"Photos of the finished built-in wardrobe 📷", "Hello,\n\nthe wardrobe turned out great, here are a few photos.\nMay we recommend you?\n\nWarm regards"},
	{"Appointment confirmation – installation", "Good morning,\n\nwe confirm the installation on Tuesday from 8 am.\nPlease keep the hallway clear."},
	{"Samples: linoleum in sage and midnight blue", "Hello,\n\nthe samples went out today: pebble, sage and midnight blue.\nLet us know which one fits."},
	{"Question about caring for the worktop", "Hi,\n\nhow often should the worktop be re-oiled?\nAnd which oil do you recommend?\n\nRegards"},
	{"Resize the shelf – is that still possible?", "Hello,\n\ncould we make the shelf 12 cm narrower? The radiator is in the way after all.\nIf that causes extra cost, please tell us beforehand.\n\nThanks!"},
	{"Reminder: rental tool due back by Friday", "Hello,\n\nthe plunge saw is booked until Friday.\nPlease return it by 5 pm."},
	{"Happy Easter 🐣 and thank you!", "Dear workshop team,\n\nhappy Easter and many thanks for the lovely bench.\nIt now stands under the apple tree.\n\nWarmly"},
	{"Shipment delivered", "Good day,\n\nyour shipment was delivered today at 11:42 am.\nLeft at: workshop, side entrance."},
	{"Table for the daycare – height 52 or 58 cm?", "Hello,\n\nfor the nursery group we would need the lower height, for the older ones 58 cm.\nCan both come out of one board?\n\nBest wishes from the daycare"},
	{"Stand plan and set-up times", "Good day,\n\nthe stand plan is attached. Set-up is the day before, from noon to 8 pm."},
	{"Quick question about the oil-wax mix", "Hi,\n\nwhich mixing ratio do you use for ash?\nMine turns out blotchy.\n\nBest regards"},
	{"Order confirmation – window sills", "Good day,\n\nwe hereby confirm the order for six window sills in beech, 27 mm.\nCompletion in two weeks."},
	{"Key handover on Saturday?", "Hello,\n\ndoes the key handover on Saturday around 10 am work?\nOtherwise I will put it in the mailbox.\n\nRegards"},
	{"Complaint: drawer sticks", "Good day,\n\nthe bottom drawer has been sticking since the weekend.\nCould you drop by next week?\n\nKind regards"},
	{"Thanks for the quick response 🙏", "Hi,\n\nthat was fast. The door closes again, the kids are thrilled.\n\nSee you soon"},
	{"Timber list for the carport", "Hello,\n\nattached is the timber list: KVH 8/16, 6/12 and cladding 22 mm.\nPlease check that the quantities are right."},
	{"Invitation: open workshop day", "Dear neighbours,\n\non the 18th we open the workshop, there will be waffles and sawdust.\nDo drop by!"},
	{"Reminder? No – just a friendly nudge", "Good day,\n\ninvoice 2026-0877 is still open.\nIf it has already been paid: please ignore this.\n\nFriendly regards"},
}

// threads are conversations that run back and forth for a while.
var threads = []struct {
	with    int // index into people
	subject string
	steps   int
}{
	{2, "Kitchen in old building: measuring, design, dates", 14},
	{4, "Attic stairs – stringers in oak or ash?", 10},
	{9, "Wardrobe for the dandelion group 🦁", 9},
	{12, "Studio shop counter: dimensions and finish", 8},
}

var threadLines = []string{
	"thanks for the quick reply. Then we will do it that way.",
	"I measured again: it is 3.42 m, not 3.40 m.",
	"can we move the appointment to Thursday? The electrician is here on Wednesday.",
	"we like the sketch a lot. Only the handles we would like in black.",
	"attached is the revised drawing with the new dimensions.",
	"works for me. I will order the material and get back to you once it is here.",
	"one more question: will the oil hold up at the sink, or better use lacquer?",
	"wonderful, see you Thursday at eight.",
	"we have not agreed on the colour yet. May we think about it until Monday?",
	"the down payment went out today.",
}

// A valid PNG, one pixel.
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0xf0,
	0x1f, 0x00, 0x05, 0x00, 0x01, 0xff, 0x56, 0xc7, 0x2f, 0x0d, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
	0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

type rng struct{ s uint32 }

func (r *rng) next() uint32 {
	r.s += 0x6D2B79F5
	t := r.s
	t = (t ^ (t >> 15)) * (t | 1)
	t ^= t + (t^(t>>7))*(t|61)
	return t ^ (t >> 14)
}

func (r *rng) intn(n int) int { return int(r.next() % uint32(n)) }

// csv returns about size bytes of a plausible measurement table.
func csv(r *rng, size int) []byte {
	var b bytes.Buffer
	b.WriteString("Position;Description;Length mm;Width mm;Thickness mm;Pieces;Unit price €\n")
	for i := 1; b.Len() < size; i++ {
		fmt.Fprintf(&b, "%d;Cut piece %c%d;%d;%d;%d;%d;%d,%02d\n", i, 'A'+rune(r.intn(26)), r.intn(90)+10,
			300+r.intn(2400), 80+r.intn(700), []int{12, 16, 19, 22, 27, 40}[r.intn(6)], 1+r.intn(12), 3+r.intn(140), r.intn(100))
	}
	return b.Bytes()
}

func html(text string) string {
	var b strings.Builder
	b.WriteString("<html><body style=\"font-family: sans-serif\">\n")
	for _, para := range strings.Split(text, "\n\n") {
		b.WriteString("<p>" + strings.ReplaceAll(para, "\n", "<br>\n") + "</p>\n")
	}
	b.WriteString("</body></html>")
	return b.String()
}

type planned struct {
	folder string
	when   time.Time
	spec   edgestub.MailSpec
	flags  []string
	unread bool
}

// seed fills one mailbox with inbox mails in INBOX and sent in Sent, over
// the last half year, denser towards today.
func seed(stub *edgestub.Stub, address string, n int, all []account, inbox, sent int) {
	r := &rng{s: uint32(7919 * (n + 1))}
	now := time.Now().UTC().Truncate(time.Minute)
	local := strings.SplitN(address, "@", 2)[0]
	me := person{strings.ToUpper(local[:1]) + local[1:] + " (Workshop)", address}
	id := func(kind string, i int) string { return fmt.Sprintf("%s-%s-%d@mail.werkstatt.test", kind, local, i) }
	// age of the i-th of total mails: half a year down to minutes
	at := func(i, total int) time.Time {
		f := math.Pow(float64(total-i)/float64(total), 1.7)
		return now.Add(-time.Duration(f*180*24*float64(time.Hour)) - time.Duration(20+r.intn(40))*time.Minute)
	}
	var plan []planned

	// conversations: they take their share of both folders first
	threadIn, threadOut := 0, 0
	if inbox >= 100 {
		for ti, th := range threads {
			other := people[th.with]
			var chain []string
			start := now.Add(-time.Duration(20+ti*35) * 24 * time.Hour)
			for step := 0; step < th.steps; step++ {
				incoming := step%2 == 0
				when := start.Add(time.Duration(step)*26*time.Hour + time.Duration(r.intn(300))*time.Minute)
				mid := id(fmt.Sprintf("thread%d", ti), step+1)
				spec := edgestub.MailSpec{
					Subject: th.subject, Date: when, MessageID: mid,
					Text: "Hello,\n\n" + threadLines[(ti*3+step)%len(threadLines)] + "\n\nBest regards",
				}
				if step > 0 {
					spec.Subject = "Re: " + th.subject
					spec.InReplyTo = chain[len(chain)-1]
					spec.References = append([]string(nil), chain...)
				}
				if incoming {
					spec.From, spec.To = other.String(), []string{me.String()}
					if step%4 == 2 {
						spec.HTML = html(spec.Text)
					}
					plan = append(plan, planned{folder: edgestub.Inbox, when: when, spec: spec, flags: []string{`\Answered`}})
					threadIn++
				} else {
					spec.From, spec.To = me.String(), []string{other.String()}
					plan = append(plan, planned{folder: edgestub.Sent, when: when, spec: spec})
					threadOut++
				}
				chain = append(chain, mid)
			}
		}
	}

	// single incoming mails
	singles := inbox - threadIn
	bigAttachments := map[int]int{singles / 5: 2 << 20, singles / 2: 1200 << 10, singles * 3 / 4: 800 << 10, singles - 9: 1600 << 10}
	for i := 0; i < singles; i++ {
		topic := topics[(i*7+n*3+r.intn(3))%len(topics)]
		from := people[(i*5+r.intn(4))%len(people)]
		when := at(i, singles)
		spec := edgestub.MailSpec{
			From: from.String(), To: []string{me.String()},
			Subject: topic.subject, Date: when, Text: topic.text, MessageID: id("inbound", i+1),
		}
		var flags []string
		switch k := r.intn(100); {
		case k < 30:
			spec.HTML = html(topic.text)
		case k < 34:
			spec.Cc = []string{people[(i+3)%len(people)].String()}
		}
		if size, ok := bigAttachments[i]; ok && inbox >= 100 {
			spec.AttachName, spec.AttachType, spec.AttachData = fmt.Sprintf("measurements-%d.csv", i+1), "text/csv", csv(r, size)
			spec.Subject += " (with measurements)"
		} else if i%13 == 5 {
			switch i % 3 {
			case 0:
				spec.AttachName, spec.AttachType, spec.AttachData = "parts-list.csv", "text/csv", csv(r, 4000+r.intn(180000))
			case 1:
				spec.AttachName, spec.AttachType, spec.AttachData = "foto.png", "image/png", tinyPNG
				spec.HTML = html(topic.text)
			default:
				spec.AttachName, spec.AttachType = "notes.txt", "text/plain"
				spec.AttachData = []byte(strings.Repeat("Dimensions checked, edges eased, oiled twice.\n", 40+r.intn(400)))
			}
		}
		if i%17 == 4 {
			flags = append(flags, `\Flagged`)
		}
		if i%29 == 7 {
			flags = append(flags, "$Forwarded")
		}
		if i%11 == 3 {
			flags = append(flags, `\Answered`)
		}
		plan = append(plan, planned{folder: edgestub.Inbox, when: when, spec: spec, flags: flags})
	}

	// single sent mails
	for i := 0; i < sent-threadOut; i++ {
		topic := topics[(i*3+n)%len(topics)]
		to := people[(i*7+n)%len(people)].String()
		if i == 0 && len(all) > 1 {
			to = all[(n+1)%len(all)].address
		}
		when := at(i, sent-threadOut)
		plan = append(plan, planned{folder: edgestub.Sent, when: when, spec: edgestub.MailSpec{
			From: me.String(), To: []string{to}, Subject: "Re: " + topic.subject, Date: when,
			Text:      "Hi,\n\nthanks for the message, that works.\nI will get back to you with the details.\n\nBest regards from the workshop",
			MessageID: id("sent", i+1),
		}})
	}

	// one tenth of the inbox is unread: mostly the newest, a few older ones
	sort.SliceStable(plan, func(i, j int) bool { return plan[i].when.Before(plan[j].when) })
	var inboxIdx []int
	for i := range plan {
		if plan[i].folder == edgestub.Inbox {
			inboxIdx = append(inboxIdx, i)
		}
	}
	unread := len(inboxIdx) / 10
	for k := 0; k < unread; k++ {
		pos := len(inboxIdx) - 1 - k - k/3 // the newest, with gaps
		if k%5 == 4 {
			pos = r.intn(len(inboxIdx) * 3 / 4) // an old one that was never opened
		}
		for pos >= 0 && plan[inboxIdx[pos]].unread {
			pos--
		}
		if pos >= 0 {
			plan[inboxIdx[pos]].unread = true
		}
	}
	for _, p := range plan {
		flags := p.flags
		if !p.unread {
			flags = append(flags, `\Seen`)
		} else {
			flags = nil // unread and neither answered nor flagged
		}
		stub.Deliver(address, p.folder, edgestub.BuildMail(p.spec), p.when, flags...)
	}
}
