package main

// Text and JSON output (§5).
//
// These messages are the product. §8.1: "The exact bytes of every failure
// message, because those messages are the product. A reworded diagnostic is a
// contract change." So every one of them is goldened in testdata/, and the
// text and JSON forms render from one value rather than being assembled
// separately, because §5.2 promises that "--json carries everything the text
// does, so a harness never parses the text".
//
// The JSON failure shape has a second consumer that does not exist yet: §10
// says an MCP server will return it, "which is why that shape is specified
// now".
