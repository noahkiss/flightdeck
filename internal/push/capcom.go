package push

// KindCapcom is the push kind of a Capcom message (capcom-service.md 7.6): a communication
// notification from "Capcom" with an inline Reply. A device gets it only when it named the
// kind in push.register.
const KindCapcom = "capcom"

// CategoryCapcomReply is the Capcom message's category: one Reply action with a text field.
// The app sends the text with session.send to the notice's session.
const CategoryCapcomReply = "capcom-reply"

// The Capcom message's budget (app.md 16.2): the reply text, cut.
const (
	capcomBodyWords = 40
	capcomBodyChars = 240
)

// CapcomMessage is the notice for one Capcom message in the thread (capcom-service.md 7.6):
// titled "Capcom", the reply text cut to its budget, collapsed by item id so a repeat replaces
// the banner. A message from an event turn is passive; any other is active. session is the
// service session: a device whose Capcom view is in focus gets no push (app.md 8.3), and the
// app's Reply goes to it.
// marks are data sources' marks the message shows (capcom-service.md 8.3: Apple Weather); each
// goes on its own line after the cut text, so the budget never cuts one off.
func CapcomMessage(item, session, text string, event bool, marks ...string) Notice {
	body := Fit(text, capcomBodyWords, capcomBodyChars)
	for _, m := range marks {
		body += "\n" + m
	}
	no := Notice{Kind: KindCapcom, Title: "Capcom", Body: body, Level: LevelActive,
		Session: session, Collapse: collapseID("capcom-", item), Category: CategoryCapcomReply, Item: item}
	if event {
		no.Level = LevelPassive
	}
	return no
}

// CategoryCapcomCard is a calendar or reminder card's category (capcom-service.md 8.5): one Add
// action, which the app runs by writing the event on the device that taps it, then answering
// the card with capcom.card.answer.
const CategoryCapcomCard = "capcom-card"

// CapcomCard is the notice for a calendar or reminder card in the thread: titled "Capcom", the
// card's line as the body, collapsed by item id, with the Add action.
func CapcomCard(item, session, text string, event bool) Notice {
	no := CapcomMessage(item, session, text, event)
	no.Category = CategoryCapcomCard
	return no
}
