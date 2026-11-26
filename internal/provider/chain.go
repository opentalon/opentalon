package provider

// Chain returns the models that can answer a request for ref on p, in the
// order p tries them: the model of another configured provider ref selects,
// then the default model, then the endpoints that model falls back to. model
// is the model a request carries when ref selects no other provider's.
func Chain(p Provider, ref ModelRef, model string) []ModelInfo {
	switch p := p.(type) {
	case *selectingProvider:
		var out []ModelInfo
		if sel, m, _ := p.lookup(string(ref)); sel != nil {
			out = append(out, m)
		}
		return append(out, Chain(p.def, "", p.model)...)
	case *healthGatedProvider:
		out := make([]ModelInfo, 0, len(p.entries))
		for _, e := range p.entries {
			served, _ := p.served(e, model)
			if m := findModel(e.Prov.Models(), served); m.ID != "" {
				out = append(out, m)
			}
		}
		return out
	}
	if m := findModel(p.Models(), model); m.ID != "" {
		return []ModelInfo{m}
	}
	return nil
}
