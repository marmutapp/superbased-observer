// median.js — return the median of an array of numbers.
//
// Contract (preserved by any fix): `median(numbers)` is exported and
// returns a number. The semantic assertion checks this survives.

function median(nums) {
  const sorted = [...nums].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  // BUG: for even-length input the median is the AVERAGE of the two
  // middle values, not the upper-middle element. This returns the wrong
  // value for even-length arrays (e.g. median([1,2,3,4]) yields 3, not
  // 2.5). The minimal fix handles the even case; do not rewrite the sort
  // or change the export.
  return sorted[mid];
}

module.exports = { median };
