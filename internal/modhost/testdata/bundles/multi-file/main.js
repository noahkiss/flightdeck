// Requires files of the bundle, and renders from an async function.

var fmt = require("./lib/format.js");
var again = require("./lib/format");

exports.render = async function (ctx) {
  var label = await Promise.resolve(fmt.label("long text", 200));
  return { type: "metric", label: label, value: again === fmt ? "same" : "different", unit: fmt.unit };
};

exports.act = async function (name, args, ctx) {
  if (name === "fail.later") {
    await null;
    throw new TypeError("failed after await");
  }
  if (name === "require.outside") {
    require("../../escape.js");
  }
  if (name === "require.bare") {
    require("fs");
  }
};
