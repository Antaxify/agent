package symtab

import (
	"bytes"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/go-kit/log"
	"golang.org/x/exp/slices"
)

var (
	errNoBuildID       = fmt.Errorf(".note.gnu.build-id section not found")
	errElfBaseNotFound = fmt.Errorf("elf base not found")
	errNoDebugLink     = fmt.Errorf(".gnu_debuglink section not found")
)

type ElfTable struct {
	fs string
	//symbolFile  string
	elfFilePath string
	table       SymbolNameResolver
	base        uint64

	loaded bool
	err    error

	options ElfTableOptions
	logger  log.Logger
	procMap *ProcMap
}

type ElfTableOptions struct {
	ElfCache *ElfCache
}

func NewElfTable(logger log.Logger, procMap *ProcMap, fs string, elfFilePath string, options ElfTableOptions) *ElfTable {
	res := &ElfTable{
		procMap:     procMap,
		fs:          fs,
		elfFilePath: elfFilePath,
		logger:      logger,
		options:     options,
		table:       &noopSymbolNameResolver{},
	}
	return res
}

func (p *ElfTable) findBase(e *MMapedElfFile) bool {
	m := p.procMap
	if e.FileHeader.Type == elf.ET_EXEC {
		p.base = 0
		return true
	}
	for _, prog := range e.Progs {
		if prog.Type == elf.PT_LOAD && (prog.Flags&elf.PF_X != 0) {
			if uint64(m.Offset) == prog.Off {
				p.base = m.StartAddr - prog.Vaddr
				return true
			}
		}
	}
	return false
}

func (t *ElfTable) load() {
	if t.loaded {
		return
	}
	t.loaded = true
	fsElfFilePath := path.Join(t.fs, t.elfFilePath)

	me, err := NewMMapedElfFile(fsElfFilePath)
	if err != nil {
		t.err = err
		return
	}
	defer func() {
		if t.table != me {
			me.close()
		}
	}()

	if !t.findBase(me) {
		t.err = errElfBaseNotFound
		return
	}
	buildID, err := getBuildID(me)

	symbols := t.options.ElfCache.GetSymbolsByBuildID(buildID)
	if symbols != nil {
		t.table = symbols
		return
	}

	fileInfo, err := os.Stat(path.Join(t.fs, t.elfFilePath))
	if err != nil {
		t.err = err
		return
	}
	symbols = t.options.ElfCache.GetSymbolsByStat(statFromFileInfo(fileInfo))
	if symbols != nil {
		t.table = symbols
		return
	}

	debugFilePath, debugFileStat := t.findDebugFile(buildID, me)
	if debugFilePath != "" {
		symbols = t.options.ElfCache.GetSymbolsByStat(debugFileStat)
		if symbols != nil {
			t.table = symbols
			return
		}
		debugMe, err := NewMMapedElfFile(path.Join(t.fs, debugFilePath))
		if err != nil {
			t.err = err
			return
		}
		defer func() {
			if t.table != debugMe {
				debugMe.close()
			}
		}()
		err = debugMe.readSymbols()
		if err != nil {
			t.err = nil
			return
		}
		t.table = debugMe
		t.options.ElfCache.CacheByBuildID(buildID, debugMe)
		t.options.ElfCache.CacheByStat(debugFileStat, debugMe)
		return
	}

	err = me.readSymbols()
	if err != nil {
		t.err = err
		return
	}

	t.options.ElfCache.CacheByBuildID(buildID, me)
	t.options.ElfCache.CacheByStat(statFromFileInfo(fileInfo), me)
	t.table = me
	return

}

func (t *ElfTable) Resolve(pc uint64) string {
	t.load()
	pc -= t.base
	return t.table.Resolve(pc)
}

func (t *ElfTable) Cleanup() {
	if t.table != nil {
		t.table.Cleanup()
	}
}

//func getElfSymbols(elfPath string, elfFile *elf.File) []Sym {
//	symtab := getELFSymbolsFromSymtab(elfPath, elfFile)
//	if len(symtab) > 0 {
//		return symtab
//	}
//	//pclntab, err := getELFSymbolsFromPCLN(elfPath, elfFile)
//	//if err != nil {
//	//	return symtab
//	//}
//	//return pclntab
//	return nil
//}

func getELFSymbolsFromSymtab(elfPath string, elfFile *elf.File) []Sym {
	symtab, _ := elfFile.Symbols()
	dynsym, _ := elfFile.DynamicSymbols()
	var symbols []Sym
	add := func(t []elf.Symbol) {
		for _, sym := range t {
			if sym.Value != 0 && sym.Info&0xf == byte(elf.STT_FUNC) {
				symbols = append(symbols, Sym{
					Name:  sym.Name,
					Start: sym.Value,
					//Module: elfPath,
				})
			}
		}
	}
	add(symtab)
	add(dynsym)
	slices.SortFunc(symbols, func(a, b Sym) bool {
		if a.Start == b.Start {
			return strings.Compare(a.Name, b.Name) < 0
		}
		return a.Start < b.Start
	})
	return symbols
}

func getBuildID(elfFile *MMapedElfFile) (string, error) {
	buildIDSection := elfFile.Section(".note.gnu.build-id")
	if buildIDSection == nil {
		return "", errNoBuildID
	}

	data, err := elfFile.SectionData(buildIDSection)
	if err != nil {
		return "", fmt.Errorf("reading .note.gnu.build-id %w", err)
	}
	if len(data) < 16 {
		return "", fmt.Errorf(".note.gnu.build-id is too small")
	}
	if !bytes.Equal([]byte("GNU"), data[12:15]) {
		return "", fmt.Errorf(".note.gnu.build-id is not a GNU build-id")
	}
	buildID := hex.EncodeToString(data[16:])
	return buildID, nil
}

func (t *ElfTable) findDebugFileWithBuildID(buildID string) (string, stat) {
	if len(buildID) < 3 {
		return "", stat{}
	}

	debugFile := fmt.Sprintf("/usr/lib/debug/.build-id/%s/%s.debug", buildID[:2], buildID[2:])
	fsDebugFile := path.Join(t.fs, debugFile)
	fileInfo, err := os.Stat(fsDebugFile)
	if err == nil {
		return debugFile, statFromFileInfo(fileInfo)
	}

	return "", stat{}
}

func (t *ElfTable) findDebugFile(buildID string, elfFile *MMapedElfFile) (string, stat) {
	// https://sourceware.org/gdb/onlinedocs/gdb/Separate-Debug-Files.html
	// So, for example, suppose you ask GDB to debug /usr/bin/ls, which has a debug link that specifies the file
	// ls.debug, and a build ID whose value in hex is abcdef1234. If the list of the global debug directories
	// includes /usr/lib/debug, then GDB will look for the following debug information files, in the indicated order:
	//
	//- /usr/lib/debug/.build-id/ab/cdef1234.debug
	//- /usr/bin/ls.debug
	//- /usr/bin/.debug/ls.debug
	//- /usr/lib/debug/usr/bin/ls.debug.
	debugFile, fileInfo := t.findDebugFileWithBuildID(buildID)
	if debugFile != "" {
		return debugFile, fileInfo
	}
	debugFile, fileInfo, _ = t.findDebugFileWithDebugLink(elfFile)
	return debugFile, fileInfo
}

func (t *ElfTable) findDebugFileWithDebugLink(elfFile *MMapedElfFile) (string, stat, error) {
	fs := t.fs
	elfFilePath := t.elfFilePath
	debugLinkSection := elfFile.Section(".gnu_debuglink")
	if debugLinkSection == nil {
		return "", stat{}, errNoDebugLink
	}
	data, err := elfFile.SectionData(debugLinkSection)
	if err != nil {
		return "", stat{}, fmt.Errorf("reading .gnu_debuglink %w", err)
	}
	if len(data) < 6 {
		return "", stat{}, fmt.Errorf(".gnu_debuglink is too small")
	}
	crc := data[len(data)-4:]
	_ = crc
	debugLink := cString(data)

	// /usr/bin/ls.debug
	fsDebugFile := path.Join(path.Dir(elfFilePath), debugLink)
	fileInfo, err := os.Stat(path.Join(fs, fsDebugFile))
	if err == nil {
		return fsDebugFile, statFromFileInfo(fileInfo), nil
	}
	// /usr/bin/.debug/ls.debug
	fsDebugFile = path.Join(path.Dir(elfFilePath), ".debug", debugLink)
	fileInfo, err = os.Stat(path.Join(fs, fsDebugFile))
	if err == nil {
		return fsDebugFile, statFromFileInfo(fileInfo), nil
	}
	// /usr/lib/debug/usr/bin/ls.debug.
	fsDebugFile = path.Join("/usr/lib/debug", path.Dir(elfFilePath), debugLink)
	fileInfo, err = os.Stat(path.Join(fs, fsDebugFile))
	if err == nil {
		return fsDebugFile, statFromFileInfo(fileInfo), nil
	}

	return "", stat{}, nil
}

func cString(bs []byte) string {
	i := 0
	for ; i < len(bs); i++ {
		if bs[i] == 0 {
			break
		}
	}
	return string(bs[:i])
}
