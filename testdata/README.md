# testdata

Golden patches and golden reports.

The reports here are a contract, not a convenience. SPEC §8.1: "The exact bytes
of every failure message, because those messages are the product. A reworded
diagnostic is a contract change." A change to any golden in this directory is a
change to what an agent reads and acts on, so it gets said out loud rather than
regenerated quietly.

The JSON failure shape is goldened for a second reason: an MCP server that does
not exist yet is specified to return it.
