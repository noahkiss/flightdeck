// Keeps every string it makes, so the heap grows until the runtime stops the call.

var kept = [];

exports.render = function (ctx) {
  return { type: "text", text: "kept " + kept.length };
};

exports.refresh = function (ctx) {
  for (var i = 0; ; i++) {
    kept.push("x".repeat(1 << 16) + i);
  }
};
