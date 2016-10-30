// Copyright (c) 2016, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package syntax

import (
	"bufio"
	"io"
	"sync"
)

// Config controls how the printing of an AST node will behave.
type Config struct {
	Spaces int // 0 (default) for tabs, >0 for number of spaces
}

var printerFree = sync.Pool{
	New: func() interface{} {
		return &printer{bufWriter: bufio.NewWriter(nil)}
	},
}

// Fprint "pretty-prints" the given AST file to the given writer.
func (c Config) Fprint(w io.Writer, f *File) error {
	p := printerFree.Get().(*printer)
	p.reset()
	p.f, p.c = f, c
	p.comments = f.Comments
	p.bufWriter.Reset(w)
	p.stmts(f.Stmts)
	p.commentsUpTo(0)
	p.newline(0)
	err := p.bufWriter.Flush()
	printerFree.Put(p)
	return err
}

const maxPos = Pos(^uint(0) >> 1)

// Fprint "pretty-prints" the given AST file to the given writer. It
// calls Config.Fprint with its default settings.
func Fprint(w io.Writer, f *File) error {
	return Config{}.Fprint(w, f)
}

type bufWriter interface {
	WriteByte(byte) error
	WriteString(string) (int, error)
	Reset(io.Writer)
	Flush() error
}

type printer struct {
	bufWriter

	f *File
	c Config

	wantSpace   bool
	wantNewline bool

	commentPadding int

	// nline is the position of the next newline
	nline      Pos
	nlineIndex int

	// lastLevel is the last level of indentation that was used.
	lastLevel int
	// level is the current level of indentation.
	level int
	// levelIncs records which indentation level increments actually
	// took place, to revert them once their section ends.
	levelIncs []bool

	nestedBinary bool

	// comments is the list of pending comments to write.
	comments []*Comment

	// pendingHdocs is the list of pending heredocs to write.
	pendingHdocs []*Redirect

	// used in stmtLen to align comments
	lenPrinter *printer
	lenCounter byteCounter
}

func (p *printer) reset() {
	p.wantSpace, p.wantNewline = false, false
	p.commentPadding = 0
	p.nline, p.nlineIndex = 0, 0
	p.lastLevel, p.level = 0, 0
	p.levelIncs = p.levelIncs[:0]
	p.nestedBinary = false
	p.pendingHdocs = p.pendingHdocs[:0]
}

func (p *printer) incLine() {
	if p.nlineIndex++; p.nlineIndex >= len(p.f.Lines) {
		p.nline = maxPos
	} else {
		p.nline = Pos(p.f.Lines[p.nlineIndex])
	}
}

func (p *printer) incLines(pos Pos) {
	for p.nline < pos {
		p.incLine()
	}
}

func (p *printer) space() {
	p.WriteByte(' ')
	p.wantSpace = false
}

func (p *printer) spaces(n int) {
	for i := 0; i < n; i++ {
		p.WriteByte(' ')
	}
}

func (p *printer) tabs(n int) {
	for i := 0; i < n; i++ {
		p.WriteByte('\t')
	}
}

func (p *printer) bslashNewl() {
	p.WriteString(" \\\n")
	p.wantSpace = false
	p.incLine()
}

func (p *printer) spacedString(s string, spaceAfter bool) {
	if p.wantSpace {
		p.WriteByte(' ')
	}
	p.WriteString(s)
	p.wantSpace = spaceAfter
}

func (p *printer) semiOrNewl(s string, pos Pos) {
	if p.wantNewline {
		p.newline(pos)
		p.indent()
	} else {
		p.WriteString("; ")
	}
	p.incLines(pos)
	p.WriteString(s)
	p.wantSpace = true
}

func (p *printer) incLevel() {
	inc := false
	if p.level <= p.lastLevel {
		p.level++
		inc = true
	} else if last := &p.levelIncs[len(p.levelIncs)-1]; *last {
		*last = false
		inc = true
	}
	p.levelIncs = append(p.levelIncs, inc)
}

func (p *printer) decLevel() {
	if p.levelIncs[len(p.levelIncs)-1] {
		p.level--
	}
	p.levelIncs = p.levelIncs[:len(p.levelIncs)-1]
}

func (p *printer) indent() {
	p.lastLevel = p.level
	switch {
	case p.level == 0:
	case p.c.Spaces == 0:
		p.tabs(p.level)
	case p.c.Spaces > 0:
		p.spaces(p.c.Spaces * p.level)
	}
}

func (p *printer) newline(pos Pos) {
	p.wantNewline, p.wantSpace = false, false
	p.WriteByte('\n')
	if pos > p.nline {
		p.incLine()
	}
	hdocs := p.pendingHdocs
	p.pendingHdocs = p.pendingHdocs[:0]
	for _, r := range hdocs {
		p.word(r.Hdoc)
		p.incLines(r.Hdoc.End() + 1)
		p.unquotedWord(r.Word)
		p.WriteByte('\n')
		p.incLine()
		p.wantSpace = false
	}
}

func (p *printer) newlines(pos Pos) {
	p.newline(pos)
	if pos > p.nline {
		// preserve single empty lines
		p.WriteByte('\n')
		p.incLine()
	}
	p.indent()
}

func (p *printer) commentsAndSeparate(pos Pos) {
	p.commentsUpTo(pos)
	if p.wantNewline || pos > p.nline {
		p.newlines(pos)
	}
}

func (p *printer) sepTok(s string, pos Pos) {
	p.level++
	p.commentsUpTo(pos)
	p.level--
	if p.wantNewline || pos > p.nline {
		p.newlines(pos)
	}
	p.WriteString(s)
	p.wantSpace = true
}

func (p *printer) semiRsrv(s string, pos Pos, fallback bool) {
	p.level++
	p.commentsUpTo(pos)
	p.level--
	if p.wantNewline || pos > p.nline {
		p.newlines(pos)
	} else if fallback {
		p.WriteString("; ")
	} else if p.wantSpace {
		p.WriteByte(' ')
	}
	p.WriteString(s)
	p.wantSpace = true
}

func (p *printer) commentsUpTo(pos Pos) {
	if len(p.comments) < 1 {
		return
	}
	c := p.comments[0]
	if pos > 0 && c.Hash >= pos {
		return
	}
	p.comments = p.comments[1:]
	switch {
	case p.nlineIndex == 0:
	case c.Hash >= p.nline:
		p.newlines(c.Hash)
	default:
		p.spaces(p.commentPadding + 1)
	}
	p.incLines(c.Hash)
	p.WriteByte('#')
	p.WriteString(c.Text)
	p.commentsUpTo(pos)
}

func (p *printer) expansionOp(tok Token) {
	switch tok {
	case COLON:
		p.WriteByte(':')
	case ADD:
		p.WriteByte('+')
	case CADD:
		p.WriteString(":+")
	case SUB:
		p.WriteByte('-')
	case CSUB:
		p.WriteString(":-")
	case QUEST:
		p.WriteByte('?')
	case CQUEST:
		p.WriteString(":?")
	case ASSIGN:
		p.WriteByte('=')
	case CASSIGN:
		p.WriteString(":=")
	case REM:
		p.WriteByte('%')
	case DREM:
		p.WriteString("%%")
	case HASH:
		p.WriteByte('#')
	case DHASH:
		p.WriteString("##")
	case XOR:
		p.WriteByte('^')
	case DXOR:
		p.WriteString("^^")
	case COMMA:
		p.WriteByte(',')
	default: // DCOMMA
		p.WriteString(",,")
	}
}

func (p *printer) wordPart(wp WordPart) {
	switch x := wp.(type) {
	case *Lit:
		p.WriteString(x.Value)
	case *SglQuoted:
		if x.Quote == DOLLSQ {
			p.WriteByte('$')
		}
		p.WriteByte('\'')
		p.WriteString(x.Value)
		p.WriteByte('\'')
		p.incLines(x.End())
	case *DblQuoted:
		if x.Quote == DOLLDQ {
			p.WriteByte('$')
		}
		p.WriteByte('"')
		for i, n := range x.Parts {
			p.wordPart(n)
			if i == len(x.Parts)-1 {
				p.incLines(n.End())
			}
		}
		p.WriteByte('"')
	case *CmdSubst:
		p.incLines(x.Pos())
		p.WriteString("$(")
		p.wantSpace = len(x.Stmts) > 0 && startsWithLparen(x.Stmts[0])
		p.nestedStmts(x.Stmts, x.Right)
		p.sepTok(")", x.Right)
	case *ParamExp:
		if x.Short {
			p.WriteByte('$')
			p.WriteString(x.Param.Value)
			break
		}
		p.WriteString("${")
		if x.Length {
			p.WriteByte('#')
		}
		p.WriteString(x.Param.Value)
		if x.Ind != nil {
			p.WriteByte('[')
			p.word(x.Ind.Word)
			p.WriteByte(']')
		}
		if x.Repl != nil {
			if x.Repl.All {
				p.WriteByte('/')
			}
			p.WriteByte('/')
			p.word(x.Repl.Orig)
			p.WriteByte('/')
			p.word(x.Repl.With)
		} else if x.Exp != nil {
			p.expansionOp(x.Exp.Op)
			p.word(x.Exp.Word)
		}
		p.WriteByte('}')
	case *ArithmExp:
		p.WriteString("$((")
		p.arithm(x.X, false, false)
		p.WriteString("))")
	case *ArrayExpr:
		p.wantSpace = false
		p.WriteByte('(')
		p.wordJoin(x.List, false)
		p.sepTok(")", x.Rparen)
	case *ExtGlob:
		p.wantSpace = false
		p.WriteString(x.Token.String())
		p.WriteString(x.Pattern.Value)
		p.WriteByte(')')
	case *ProcSubst:
		// avoid conflict with << and others
		if p.wantSpace {
			p.space()
		}
		if x.Op == CMDIN {
			p.WriteString("<(")
		} else { // CMDOUT
			p.WriteString(">(")
		}
		p.nestedStmts(x.Stmts, 0)
		p.WriteByte(')')
	}
	p.wantSpace = true
}

func (p *printer) loop(loop Loop) {
	switch x := loop.(type) {
	case *WordIter:
		p.WriteString(x.Name.Value)
		if len(x.List) > 0 {
			p.WriteString(" in")
			p.wordJoin(x.List, true)
		}
	case *CStyleLoop:
		p.WriteString("((")
		if x.Init == nil {
			p.WriteByte(' ')
		}
		p.arithm(x.Init, false, false)
		p.WriteString("; ")
		p.arithm(x.Cond, false, false)
		p.WriteString("; ")
		p.arithm(x.Post, false, false)
		p.WriteString("))")
	}
}

func (p *printer) binaryExprOp(tok Token) {
	switch tok {
	case ASSIGN:
		p.WriteByte('=')
	case ADD:
		p.WriteByte('+')
	case SUB:
		p.WriteByte('-')
	case REM:
		p.WriteByte('%')
	case MUL:
		p.WriteByte('*')
	case QUO:
		p.WriteByte('/')
	case AND:
		p.WriteByte('&')
	case OR:
		p.WriteByte('|')
	case LAND:
		p.WriteString("&&")
	case LOR:
		p.WriteString("||")
	case XOR:
		p.WriteByte('^')
	case POW:
		p.WriteString("**")
	case EQL:
		p.WriteString("==")
	case NEQ:
		p.WriteString("!=")
	case LEQ:
		p.WriteString("<=")
	case GEQ:
		p.WriteString(">=")
	case ADDASSGN:
		p.WriteString("+=")
	case SUBASSGN:
		p.WriteString("-=")
	case MULASSGN:
		p.WriteString("*=")
	case QUOASSGN:
		p.WriteString("/=")
	case REMASSGN:
		p.WriteString("%=")
	case ANDASSGN:
		p.WriteString("&=")
	case ORASSGN:
		p.WriteString("|=")
	case XORASSGN:
		p.WriteString("^=")
	case SHLASSGN:
		p.WriteString("<<=")
	case SHRASSGN:
		p.WriteString(">>=")
	case LSS:
		p.WriteByte('<')
	case GTR:
		p.WriteByte('>')
	case SHL:
		p.WriteString("<<")
	case SHR:
		p.WriteString(">>")
	case QUEST:
		p.WriteByte('?')
	case COLON:
		p.WriteByte(':')
	case COMMA:
		p.WriteByte(',')
	case TREMATCH:
		p.WriteString("=~")
	case TNEWER:
		p.WriteString("-nt")
	case TOLDER:
		p.WriteString("-ot")
	case TDEVIND:
		p.WriteString("-ef")
	case TEQL:
		p.WriteString("-eq")
	case TNEQ:
		p.WriteString("-ne")
	case TLEQ:
		p.WriteString("-le")
	case TGEQ:
		p.WriteString("-ge")
	case TLSS:
		p.WriteString("-lt")
	default: // TGTR
		p.WriteString("-gt")
	}
}

func (p *printer) unaryExprOp(tok Token) {
	switch tok {
	case ADD:
		p.WriteByte('+')
	case SUB:
		p.WriteByte('-')
	case NOT:
		p.WriteByte('!')
	case INC:
		p.WriteString("++")
	case DEC:
		p.WriteString("--")
	case TEXISTS:
		p.WriteString("-e")
	case TREGFILE:
		p.WriteString("-f")
	case TDIRECT:
		p.WriteString("-d")
	case TCHARSP:
		p.WriteString("-c")
	case TBLCKSP:
		p.WriteString("-b")
	case TNMPIPE:
		p.WriteString("-p")
	case TSOCKET:
		p.WriteString("-S")
	case TSMBLINK:
		p.WriteString("-L")
	case TSGIDSET:
		p.WriteString("-g")
	case TSUIDSET:
		p.WriteString("-u")
	case TREAD:
		p.WriteString("-r")
	case TWRITE:
		p.WriteString("-w")
	case TEXEC:
		p.WriteString("-x")
	case TNOEMPTY:
		p.WriteString("-s")
	case TFDTERM:
		p.WriteString("-t")
	case TEMPSTR:
		p.WriteString("-z")
	case TNEMPSTR:
		p.WriteString("-n")
	case TOPTSET:
		p.WriteString("-o")
	case TVARSET:
		p.WriteString("-v")
	default: // TNRFVAR
		p.WriteString("-R")
	}
}

func (p *printer) arithm(expr ArithmExpr, compact, test bool) {
	p.wantSpace = false
	switch x := expr.(type) {
	case *Word:
		p.word(*x)
	case *BinaryExpr:
		if compact {
			p.arithm(x.X, compact, test)
			p.binaryExprOp(x.Op)
			p.arithm(x.Y, compact, test)
		} else {
			p.arithm(x.X, compact, test)
			if x.Op != COMMA {
				p.WriteByte(' ')
			}
			p.binaryExprOp(x.Op)
			p.space()
			p.arithm(x.Y, compact, test)
		}
	case *UnaryExpr:
		if x.Post {
			p.arithm(x.X, compact, test)
			p.unaryExprOp(x.Op)
		} else {
			p.unaryExprOp(x.Op)
			if test {
				p.space()
			}
			p.arithm(x.X, compact, test)
		}
	case *ParenExpr:
		p.WriteByte('(')
		p.arithm(x.X, false, test)
		p.WriteByte(')')
	}
}

func (p *printer) word(w Word) {
	for _, n := range w.Parts {
		p.wordPart(n)
	}
}

func (p *printer) unquotedWord(w Word) {
	for _, wp := range w.Parts {
		switch x := wp.(type) {
		case *SglQuoted:
			p.WriteString(x.Value)
		case *DblQuoted:
			for _, qp := range x.Parts {
				p.wordPart(qp)
			}
		case *Lit:
			if x.Value[0] == '\\' {
				p.WriteString(x.Value[1:])
			} else {
				p.WriteString(x.Value)
			}
		default:
			p.wordPart(wp)
		}
	}
}

func (p *printer) wordJoin(ws []Word, backslash bool) {
	anyNewline := false
	for _, w := range ws {
		if pos := w.Pos(); pos > p.nline {
			p.commentsUpTo(pos)
			if backslash {
				p.bslashNewl()
			} else {
				p.WriteByte('\n')
				p.incLine()
			}
			if !anyNewline {
				p.incLevel()
				anyNewline = true
			}
			p.indent()
		} else if p.wantSpace {
			p.space()
		}
		for _, n := range w.Parts {
			p.wordPart(n)
		}
	}
	if anyNewline {
		p.decLevel()
	}
}

func (p *printer) stmt(s *Stmt) {
	if s.Negated {
		p.spacedString("!", true)
	}
	p.assigns(s.Assigns)
	startRedirs := p.command(s.Cmd, s.Redirs)
	anyNewline := false
	for _, r := range s.Redirs[startRedirs:] {
		if r.OpPos > p.nline {
			p.bslashNewl()
			if !anyNewline {
				p.incLevel()
				anyNewline = true
			}
			p.indent()
		}
		p.commentsAndSeparate(r.OpPos)
		if p.wantSpace {
			p.WriteByte(' ')
		}
		if r.N != nil {
			p.WriteString(r.N.Value)
		}
		p.redirectOp(r.Op)
		p.wantSpace = true
		p.word(r.Word)
		if r.Op == SHL || r.Op == DHEREDOC {
			p.pendingHdocs = append(p.pendingHdocs, r)
		}
	}
	if anyNewline {
		p.decLevel()
	}
	if s.Background {
		p.WriteString(" &")
	}
}

func (p *printer) redirectOp(tok Token) {
	switch tok {
	case LSS:
		p.WriteByte('<')
	case GTR:
		p.WriteByte('>')
	case SHL:
		p.WriteString("<<")
	case SHR:
		p.WriteString(">>")
	case RDRINOUT:
		p.WriteString("<>")
	case DPLIN:
		p.WriteString("<&")
	case DPLOUT:
		p.WriteString(">&")
	case CLBOUT:
		p.WriteString(">|")
	case DHEREDOC:
		p.WriteString("<<-")
	case WHEREDOC:
		p.WriteString("<<<")
	case RDRALL:
		p.WriteString("&>")
	default: // APPALL
		p.WriteString("&>>")
	}
}

func binaryCmdOp(tok Token) string {
	switch tok {
	case OR:
		return "|"
	case LAND:
		return "&&"
	case LOR:
		return "||"
	default: // PIPEALL
		return "|&"
	}
}

func caseClauseOp(tok Token) string {
	switch tok {
	case DSEMICOLON:
		return ";;"
	case SEMIFALL:
		return ";&"
	default: // DSEMIFALL
		return ";;&"
	}
}

func (p *printer) command(cmd Command, redirs []*Redirect) (startRedirs int) {
	switch x := cmd.(type) {
	case *CallExpr:
		if len(x.Args) <= 1 {
			p.wordJoin(x.Args, true)
			return 0
		}
		p.wordJoin(x.Args[:1], true)
		for _, r := range redirs {
			if r.Pos() > x.Args[1].Pos() || r.Op == SHL || r.Op == DHEREDOC {
				break
			}
			if p.wantSpace {
				p.space()
			}
			if r.N != nil {
				p.WriteString(r.N.Value)
			}
			p.redirectOp(r.Op)
			p.wantSpace = true
			p.word(r.Word)
			startRedirs++
		}
		p.wordJoin(x.Args[1:], true)
	case *Block:
		p.spacedString("{", true)
		p.nestedStmts(x.Stmts, x.Rbrace)
		p.semiRsrv("}", x.Rbrace, true)
	case *IfClause:
		p.spacedString("if", true)
		p.nestedStmts(x.CondStmts, 0)
		p.semiOrNewl("then", x.Then)
		p.nestedStmts(x.ThenStmts, 0)
		for _, el := range x.Elifs {
			p.semiRsrv("elif", el.Elif, true)
			p.nestedStmts(el.CondStmts, 0)
			p.semiOrNewl("then", el.Then)
			p.nestedStmts(el.ThenStmts, 0)
		}
		if len(x.ElseStmts) > 0 {
			p.semiRsrv("else", x.Else, true)
			p.nestedStmts(x.ElseStmts, 0)
		} else if x.Else > 0 {
			p.incLines(x.Else)
		}
		p.semiRsrv("fi", x.Fi, true)
	case *Subshell:
		p.spacedString("(", false)
		p.wantSpace = len(x.Stmts) > 0 && startsWithLparen(x.Stmts[0])
		p.nestedStmts(x.Stmts, x.Rparen)
		p.sepTok(")", x.Rparen)
	case *WhileClause:
		p.spacedString("while", true)
		p.nestedStmts(x.CondStmts, 0)
		p.semiOrNewl("do", x.Do)
		p.nestedStmts(x.DoStmts, 0)
		p.semiRsrv("done", x.Done, true)
	case *ForClause:
		p.spacedString("for ", true)
		p.loop(x.Loop)
		p.semiOrNewl("do", x.Do)
		p.nestedStmts(x.DoStmts, 0)
		p.semiRsrv("done", x.Done, true)
	case *BinaryCmd:
		p.stmt(x.X)
		indent := !p.nestedBinary
		if indent {
			p.incLevel()
		}
		_, p.nestedBinary = x.Y.Cmd.(*BinaryCmd)
		if len(p.pendingHdocs) == 0 && x.Y.Pos() > p.nline {
			p.bslashNewl()
			p.indent()
		}
		p.spacedString(binaryCmdOp(x.Op), true)
		p.incLines(x.Y.Pos())
		p.stmt(x.Y)
		if indent {
			p.decLevel()
		}
		p.nestedBinary = false
	case *FuncDecl:
		if x.BashStyle {
			p.WriteString("function ")
		}
		p.WriteString(x.Name.Value)
		p.WriteString("() ")
		p.incLines(x.Body.Pos())
		p.stmt(x.Body)
	case *CaseClause:
		p.spacedString("case ", true)
		p.word(x.Word)
		p.WriteString(" in")
		p.incLevel()
		for _, pl := range x.List {
			p.commentsAndSeparate(pl.Patterns[0].Pos())
			for i, w := range pl.Patterns {
				if i > 0 {
					p.spacedString("|", true)
				}
				if p.wantSpace {
					p.WriteByte(' ')
				}
				for _, n := range w.Parts {
					p.wordPart(n)
				}
			}
			p.WriteByte(')')
			sep := len(pl.Stmts) > 1 || (len(pl.Stmts) > 0 && pl.Stmts[0].Pos() > p.nline)
			p.nestedStmts(pl.Stmts, 0)
			p.level++
			if sep {
				p.sepTok(caseClauseOp(pl.Op), pl.OpPos)
			} else {
				p.spacedString(caseClauseOp(pl.Op), true)
			}
			p.incLines(pl.OpPos)
			p.level--
			if sep || pl.OpPos == x.Esac {
				p.wantNewline = true
			}
		}
		p.decLevel()
		p.semiRsrv("esac", x.Esac, len(x.List) == 0)
	case *UntilClause:
		p.spacedString("until", true)
		p.nestedStmts(x.CondStmts, 0)
		p.semiOrNewl("do", x.Do)
		p.nestedStmts(x.DoStmts, 0)
		p.semiRsrv("done", x.Done, true)
	case *ArithmExp:
		if p.wantSpace {
			p.space()
		}
		p.WriteString("((")
		p.arithm(x.X, false, false)
		p.WriteString("))")
	case *TestClause:
		p.spacedString("[[", true)
		p.space()
		p.arithm(x.X, false, true)
		p.spacedString("]]", true)
	case *DeclClause:
		name := x.Variant
		if name == "" {
			name = "declare"
		}
		p.spacedString(name, true)
		for _, w := range x.Opts {
			p.WriteByte(' ')
			p.word(w)
		}
		p.assigns(x.Assigns)
	case *EvalClause:
		p.spacedString("eval", true)
		if x.Stmt != nil {
			p.stmt(x.Stmt)
		}
	case *CoprocClause:
		p.spacedString("coproc", true)
		if x.Name != nil {
			p.WriteByte(' ')
			p.WriteString(x.Name.Value)
		}
		p.stmt(x.Stmt)
	case *LetClause:
		p.spacedString("let", true)
		for _, n := range x.Exprs {
			p.space()
			p.arithm(n, true, false)
		}
	}
	return startRedirs
}

func startsWithLparen(s *Stmt) bool {
	switch x := s.Cmd.(type) {
	case *Subshell:
		return true
	case *BinaryCmd:
		return startsWithLparen(x.X)
	}
	return false
}

func (p *printer) hasInline(pos, npos, nline Pos) bool {
	for _, c := range p.comments {
		if c.Hash > nline {
			return false
		}
		if c.Hash > pos && (npos == 0 || c.Hash < npos) {
			return true
		}
	}
	return false
}

func (p *printer) stmts(stmts []*Stmt) {
	switch len(stmts) {
	case 0:
		return
	case 1:
		s := stmts[0]
		pos := s.Pos()
		p.commentsUpTo(pos)
		if pos <= p.nline {
			p.stmt(s)
		} else {
			if p.nlineIndex > 0 {
				p.newlines(pos)
			} else {
				p.incLines(pos)
			}
			p.stmt(s)
			p.wantNewline = true
		}
		return
	}
	inlineIndent := 0
	for i, s := range stmts {
		pos := s.Pos()
		ind := p.nlineIndex
		p.commentsUpTo(pos)
		if p.nlineIndex > 0 {
			p.newlines(pos)
		}
		p.incLines(pos)
		p.stmt(s)
		var npos Pos
		if i+1 < len(stmts) {
			npos = stmts[i+1].Pos()
		}
		if !p.hasInline(pos, npos, p.nline) {
			inlineIndent = 0
			p.commentPadding = 0
			continue
		}
		if ind < len(p.f.Lines)-1 && s.End() > Pos(p.f.Lines[ind+1]) {
			inlineIndent = 0
		}
		if inlineIndent == 0 {
			ind2 := p.nlineIndex
			nline2 := p.nline
			follow := stmts[i:]
			for j, s2 := range follow {
				pos2 := s2.Pos()
				var npos2 Pos
				if j+1 < len(follow) {
					npos2 = follow[j+1].Pos()
				}
				if pos2 > nline2 || !p.hasInline(pos2, npos2, nline2) {
					break
				}
				if l := p.stmtLen(s2); l > inlineIndent {
					inlineIndent = l
				}
				if ind2++; ind2 >= len(p.f.Lines) {
					nline2 = maxPos
				} else {
					nline2 = Pos(p.f.Lines[ind2])
				}
			}
			if ind2 == p.nlineIndex+1 {
				// no inline comments directly after this one
				continue
			}
		}
		if inlineIndent > 0 {
			p.commentPadding = inlineIndent - p.stmtLen(s)
		}
	}
	p.wantNewline = true
}

type byteCounter int

func (c *byteCounter) WriteByte(b byte) error {
	*c++
	return nil
}
func (c *byteCounter) WriteString(s string) (int, error) {
	*c += byteCounter(len(s))
	return 0, nil
}
func (c *byteCounter) Reset(io.Writer) { *c = 0 }
func (c *byteCounter) Flush() error    { return nil }

func (p *printer) stmtLen(s *Stmt) int {
	if p.lenPrinter == nil {
		p.lenPrinter = new(printer)
	}
	*p.lenPrinter = printer{bufWriter: &p.lenCounter}
	p.lenPrinter.bufWriter.Reset(nil)
	p.lenPrinter.f = p.f
	p.lenPrinter.incLines(s.Pos())
	p.lenPrinter.stmt(s)
	return int(p.lenCounter)
}

func (p *printer) nestedStmts(stmts []*Stmt, closing Pos) {
	p.incLevel()
	if len(stmts) == 1 && closing > p.nline && stmts[0].End() <= p.nline {
		p.newline(0)
		p.indent()
	}
	p.stmts(stmts)
	p.decLevel()
}

func (p *printer) assigns(assigns []*Assign) {
	anyNewline := false
	for _, a := range assigns {
		if a.Pos() > p.nline {
			p.bslashNewl()
			if !anyNewline {
				p.incLevel()
				anyNewline = true
			}
			p.indent()
		} else if p.wantSpace {
			p.space()
		}
		if a.Name != nil {
			p.WriteString(a.Name.Value)
			if a.Append {
				p.WriteByte('+')
			}
			p.WriteByte('=')
		}
		p.word(a.Value)
		p.wantSpace = true
	}
	if anyNewline {
		p.decLevel()
	}
}