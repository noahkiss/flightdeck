// Returns a different bad tree for each place.

exports.render = function (ctx) {
  switch (ctx.place) {
    case "board":
      // An unknown type and a style that does not exist.
      return { type: "stack", children: [{ type: "chart" }, { type: "text", text: "x", style: "huge" }] };
    case "host":
      return "not a block";
    case "session":
      return undefined;
    case "sheet":
      return { type: "text", text: "f", onTap: function () {} , nested: (function () { var a = []; for (var i = 0; i < 100; i++) { a = [a]; } return a; })() };
  }
};
