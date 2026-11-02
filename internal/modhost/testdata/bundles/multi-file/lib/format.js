var units = require("./units.js");

exports.label = function (text, times) {
  var s = "";
  for (var i = 0; i < times; i++) {
    s += text + " ";
  }
  return s; // far over the metric label limit; the hab cuts it
};
exports.unit = units.unit;
