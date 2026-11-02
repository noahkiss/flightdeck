// Each function loops forever unless state says otherwise; the runtime must interrupt it.

exports.render = function (ctx) {
  if (fd.state.get("calm")) {
    return { type: "text", text: "calm" };
  }
  for (;;) {}
};

exports.refresh = function (ctx) {
  while (true) {}
};

exports.act = function (name, args, ctx) {
  if (name === "calm.down") {
    fd.state.set("calm", true);
    return;
  }
  // A loop inside a catch-all try: an interrupt is not an exception the module can catch.
  for (;;) {
    try {
      for (;;) {}
    } catch (e) {}
  }
};
