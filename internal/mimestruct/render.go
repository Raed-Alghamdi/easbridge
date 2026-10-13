package mimestruct

import "github.com/emersion/go-imap/v2"

// BodyStructure renders the index as an IMAP BODYSTRUCTURE. The result matches
// what go-imap's ExtractBodyStructure returns for the same (well-formed)
// message.
func (ix *Index) BodyStructure() imap.BodyStructure { return ix.Root.render() }

func (n *Node) disposition() *imap.BodyStructureDisposition {
	if n.Disp == "" {
		return nil
	}
	return &imap.BodyStructureDisposition{Value: n.Disp, Params: n.DispParams}
}

func (n *Node) render() imap.BodyStructure {
	if n.IsMultipart() {
		bs := &imap.BodyStructureMultiPart{Subtype: n.Sub}
		for _, k := range n.Kids {
			bs.Children = append(bs.Children, k.render())
		}
		bs.Extended = &imap.BodyStructureMultiPartExt{
			Params:      n.Params,
			Disposition: n.disposition(),
			Language:    n.Lang,
			Location:    n.Loc,
		}
		return bs
	}
	bs := &imap.BodyStructureSinglePart{
		Type:        n.Type,
		Subtype:     n.Sub,
		Params:      n.Params,
		ID:          n.ID,
		Description: n.Desc,
		Encoding:    n.Enc,
		Size:        uint32(n.BE - n.HE),
	}
	if n.Msg != nil {
		bs.MessageRFC822 = &imap.BodyStructureMessageRFC822{
			Envelope:      n.Msg.Env.IMAP(),
			BodyStructure: n.Msg.Root.render(),
			NumLines:      n.Lines,
		}
	}
	if n.Type == "text" {
		bs.Text = &imap.BodyStructureText{NumLines: n.Lines}
	}
	bs.Extended = &imap.BodyStructureSinglePartExt{
		Disposition: n.disposition(),
		Language:    n.Lang,
		Location:    n.Loc,
	}
	return bs
}
