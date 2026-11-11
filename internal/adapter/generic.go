package adapter

// Generic runs any tool as typed: remotely unless [tool.X] local/remote
// say otherwise.
type Generic struct{}

func (Generic) Name() string                      { return "generic" }
func (Generic) Tools() []string                   { return nil }
func (Generic) Detect(root string) bool           { return false }
func (Generic) DefaultCaches(root string) []Cache { return nil }

func (Generic) Classify(c Command) Plan {
	return asTyped(c, subcommand(c.Args), Run)
}
