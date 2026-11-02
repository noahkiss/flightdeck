// Uses every declared power in refresh, and keeps what came back.

exports.refresh = function (ctx) {
  var out = fd.exec(["git", "status", "--short"], { cwd: "/work", timeout: 2 });
  var page = fd.fetch("https://example.invalid/status", {
    method: "post",
    headers: { "X-Probe": "1" },
    body: "ping",
  });
  var started = fd.hab.act("session.create", { project: "p1" });
  fd.state.set("seen", {
    code: out.code,
    stdout: out.stdout,
    stdout_cut: out.stdout_cut,
    status: page.status,
    type: page.headers["content-type"],
    body: page.body,
    body_cut: page.body_cut,
    session: started.id,
  });
};

exports.render = function (ctx) {
  var s = fd.state.get("seen");
  return { type: "text", text: JSON.stringify(s), style: "mono" };
};
