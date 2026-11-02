// A module that reads hab data in refresh, keeps it in state, draws it in render and counts taps
// in act.

function refresh(ctx) {
  var res = fd.hab.get("session.list", { host: ctx.host });
  var names = res.sessions.map(function (s) { return s.name; });
  fd.state.set("sessions", names);
  fd.log("refreshed", names.length, "sessions");
}

function render(ctx) {
  var names = fd.state.get("sessions") || [];
  var count = fd.state.get("count") || 0;
  var rows = names.map(function (n) {
    return { type: "row", id: "s-" + n, symbol: "terminal", title: n, tint: "working" };
  });
  var children = [
    { type: "text", text: fd.settings.greeting + " from " + ctx.host + " (" + ctx.os + ")", style: "title" },
    { type: "list", id: "sessions", children: rows, empty: "No sessions" },
  ];
  if (fd.settings.show_count) {
    children.push({ type: "badge", text: "tapped " + count, tint: "accent" });
  }
  children.push({
    type: "button", id: "tap", title: "Tap", symbol: "hand.tap",
    action: { name: "counter.tap", args: { by: 1 } },
  });
  children.push({ type: "text", text: "Drawn for " + ctx.place + " at " + ctx.now + ", vocabulary " + ctx.vocabulary, style: "caption" });
  return { type: "stack", axis: "vertical", gap: 1, children: children };
}

function act(name, args, ctx) {
  if (name === "counter.tap") {
    fd.state.set("count", (fd.state.get("count") || 0) + args.by);
  }
}

module.exports = { refresh: refresh, render: render, act: act };
