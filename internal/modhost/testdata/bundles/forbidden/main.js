// Calls that need powers the manifest does not declare (exec, act), and bridge calls render may
// not make.

exports.refresh = function (ctx) {
  // Caught: the module records the error and carries on.
  try {
    fd.hab.act("session.stop", { id: "s1" });
    fd.state.set("caught", "no error");
  } catch (e) {
    fd.state.set("caught", e.name + ": " + e.message);
  }
};

exports.act = function (name, args, ctx) {
  if (name === "run.program") {
    fd.exec(["echo", "hi"]); // not caught
  }
  if (name === "fetch.in.act") {
    fd.state.set("fetched", fd.fetch("https://example.invalid/x").status);
  }
};

exports.render = function (ctx) {
  if (fd.state.get("try_fetch")) {
    fd.fetch("https://example.invalid/x"); // declared, but render may not fetch
  }
  return { type: "text", text: fd.state.get("caught") || "nothing yet" };
};
