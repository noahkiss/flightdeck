// A sample module for gff's tests: every kind of target a card has, state kept by the bridge.

function render(ctx) {
  var n = fd.state.get("count") || 0;
  if (fd.state.get("broken")) {
    return { type: "text" }; // no text: the tree fails the hab's check
  }
  var note = fd.state.get("note");
  return {
    type: "stack",
    children: [
      { type: "metrics", children: [{ type: "metric", label: "Count", value: String(n), tint: n > 0 ? "done" : "muted" }] },
      { type: "toggle", id: "watch", title: "Watch", value: fd.state.get("watch") === true, action: { name: "watch.set" } },
      { type: "field", id: "note", placeholder: "Note", action: { name: "note.set" } },
      { type: "text", text: note ? "Note: " + note : "No note" },
      {
        type: "stack",
        axis: "horizontal",
        children: [
          { type: "button", id: "add", title: "Add one", action: { name: "counter.add", args: { by: 1 } } },
          {
            type: "button", id: "zero", title: "Zero", role: "destructive", confirm: "Set the count to zero?",
            action: { name: "counter.zero" },
          },
          { type: "button", id: "copy", title: "Copy", action: { name: "client.copy", args: { text: "count " + n } } },
          { type: "button", id: "break", title: "Break", action: { name: "counter.break" } },
        ],
      },
    ],
  };
}

function act(name, args) {
  switch (name) {
    case "counter.add":
      fd.state.set("count", (fd.state.get("count") || 0) + args.by);
      break;
    case "counter.zero":
      fd.state.set("count", 0);
      break;
    case "counter.break":
      fd.state.set("broken", true);
      break;
    case "watch.set":
      fd.state.set("watch", args.value);
      break;
    case "note.set":
      fd.state.set("note", args.text);
      break;
    default:
      throw new Error("unknown action " + name);
  }
}

module.exports = { render: render, act: act };
