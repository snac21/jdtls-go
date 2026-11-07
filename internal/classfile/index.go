package classfile

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Member struct {
	Name       string
	Descriptor string
	Access     uint16
}

type Type struct {
	Name       string
	Access     uint16
	Super      string
	Interfaces []string
	Fields     []Member
	Methods    []Member
	SourceFile string
	Origin     string
}

type Index struct {
	Types map[string]Type
}

func NewIndex() *Index { return &Index{Types: map[string]Type{}} }

func (index *Index) AddEntries(entries []string) error {
	for _, entry := range entries {
		if info, err := os.Stat(entry); err == nil && info.IsDir() {
			if err := index.addDirectory(entry); err != nil {
				return err
			}
			continue
		}
		if strings.HasSuffix(strings.ToLower(entry), ".jar") {
			if err := index.addArchive(entry, false); err != nil {
				continue
			}
		}
	}
	return nil
}

func (index *Index) AddJDK(home string) error {
	if home == "" {
		return nil
	}
	modules := filepath.Join(home, "jmods")
	entries, err := os.ReadDir(modules)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jmod" {
			continue
		}
		if err := index.addArchive(filepath.Join(modules, entry.Name()), true); err != nil {
			continue
		}
	}
	return nil
}

func (index *Index) addDirectory(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() || filepath.Ext(path) != ".class" || strings.HasSuffix(path, "module-info.class") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		parsed, err := Parse(data)
		if err != nil {
			return nil
		}
		parsed.Origin = path
		index.Types[parsed.Name] = parsed
		return nil
	})
}

func (index *Index) addArchive(path string, jmod bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var reader *zip.Reader
	if jmod {
		if len(data) < 4 || data[0] != 'J' || data[1] != 'M' {
			return fmt.Errorf("invalid jmod header: %s", path)
		}
		reader, err = zip.NewReader(bytes.NewReader(data[4:]), int64(len(data)-4))
	} else {
		reader, err = zip.NewReader(bytes.NewReader(data), int64(len(data)))
	}
	if err != nil {
		return err
	}
	for _, file := range reader.File {
		name := file.Name
		if jmod {
			if !strings.HasPrefix(name, "classes/") {
				continue
			}
			name = strings.TrimPrefix(name, "classes/")
		}
		if filepath.Ext(name) != ".class" || strings.HasSuffix(name, "module-info.class") || strings.HasPrefix(name, "META-INF/versions/") {
			continue
		}
		input, openErr := file.Open()
		if openErr != nil {
			continue
		}
		classBytes, readErr := io.ReadAll(input)
		_ = input.Close()
		if readErr != nil {
			continue
		}
		parsed, parseErr := Parse(classBytes)
		if parseErr != nil {
			continue
		}
		parsed.Origin = path + "!/" + name
		if _, exists := index.Types[parsed.Name]; !exists {
			index.Types[parsed.Name] = parsed
		}
	}
	return nil
}

func Parse(data []byte) (Type, error) {
	r := reader{data: data}
	magic, err := r.u4()
	if err != nil || magic != 0xcafebabe {
		return Type{}, errors.New("invalid Java class file")
	}
	if _, err = r.u4(); err != nil {
		return Type{}, err
	} // version
	cpCount, err := r.u2()
	if err != nil || cpCount == 0 {
		return Type{}, errors.New("invalid constant pool")
	}
	utf8 := make([]string, cpCount)
	classNameIndex := make([]uint16, cpCount)
	for i := uint16(1); i < cpCount; i++ {
		tag, tagErr := r.u1()
		if tagErr != nil {
			return Type{}, tagErr
		}
		switch tag {
		case 1:
			n, e := r.u2()
			if e != nil {
				return Type{}, e
			}
			value, e := r.take(int(n))
			if e != nil {
				return Type{}, e
			}
			utf8[i] = string(value)
		case 3, 4:
			if err := r.skip(4); err != nil {
				return Type{}, err
			}
		case 5, 6:
			if err := r.skip(8); err != nil {
				return Type{}, err
			}
			i++
		case 7:
			classNameIndex[i], err = r.u2()
			if err != nil {
				return Type{}, err
			}
		case 8, 16, 19, 20:
			if err := r.skip(2); err != nil {
				return Type{}, err
			}
		case 9, 10, 11, 12, 17, 18:
			if err := r.skip(4); err != nil {
				return Type{}, err
			}
		case 15:
			if err := r.skip(3); err != nil {
				return Type{}, err
			}
		default:
			return Type{}, fmt.Errorf("unknown constant-pool tag %d", tag)
		}
	}
	access, err := r.u2()
	if err != nil {
		return Type{}, err
	}
	thisClass, err := r.u2()
	if err != nil {
		return Type{}, err
	}
	superClass, err := r.u2()
	if err != nil {
		return Type{}, err
	}
	if int(thisClass) >= len(classNameIndex) || classNameIndex[thisClass] == 0 {
		return Type{}, errors.New("invalid this_class index")
	}
	resolveClass := func(index uint16) string {
		if index == 0 || int(index) >= len(classNameIndex) {
			return ""
		}
		nameIndex := classNameIndex[index]
		if int(nameIndex) >= len(utf8) {
			return ""
		}
		return strings.ReplaceAll(utf8[nameIndex], "/", ".")
	}
	typeInfo := Type{Name: resolveClass(thisClass), Access: access, Super: resolveClass(superClass)}
	interfaceCount, err := r.u2()
	if err != nil {
		return Type{}, err
	}
	for range interfaceCount {
		interfaceID, e := r.u2()
		if e != nil {
			return Type{}, e
		}
		typeInfo.Interfaces = append(typeInfo.Interfaces, resolveClass(interfaceID))
	}
	fields, err := readMembers(&r, utf8)
	if err != nil {
		return Type{}, err
	}
	typeInfo.Fields = fields
	methods, err := readMembers(&r, utf8)
	if err != nil {
		return Type{}, err
	}
	typeInfo.Methods = methods
	attributeCount, err := r.u2()
	if err != nil {
		return Type{}, err
	}
	for range attributeCount {
		nameIndex, e := r.u2()
		if e != nil {
			return Type{}, e
		}
		length, e := r.u4()
		if e != nil {
			return Type{}, e
		}
		attribute, e := r.take(int(length))
		if e != nil {
			return Type{}, e
		}
		if int(nameIndex) < len(utf8) && utf8[nameIndex] == "SourceFile" && len(attribute) == 2 {
			sourceIndex := binary.BigEndian.Uint16(attribute)
			if int(sourceIndex) < len(utf8) {
				typeInfo.SourceFile = utf8[sourceIndex]
			}
		}
	}
	return typeInfo, nil
}

func readMembers(r *reader, utf8 []string) ([]Member, error) {
	count, err := r.u2()
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, count)
	for range count {
		access, e := r.u2()
		if e != nil {
			return nil, e
		}
		nameIndex, e := r.u2()
		if e != nil {
			return nil, e
		}
		descriptorIndex, e := r.u2()
		if e != nil {
			return nil, e
		}
		attributeCount, e := r.u2()
		if e != nil {
			return nil, e
		}
		member := Member{Access: access}
		if int(nameIndex) < len(utf8) {
			member.Name = utf8[nameIndex]
		}
		if int(descriptorIndex) < len(utf8) {
			member.Descriptor = utf8[descriptorIndex]
		}
		for range attributeCount {
			if _, e := r.u2(); e != nil {
				return nil, e
			}
			length, e := r.u4()
			if e != nil {
				return nil, e
			}
			if e := r.skip(int(length)); e != nil {
				return nil, e
			}
		}
		out = append(out, member)
	}
	return out, nil
}

type reader struct {
	data   []byte
	offset int
}

func (r *reader) take(size int) ([]byte, error) {
	if size < 0 || r.offset+size > len(r.data) {
		return nil, io.ErrUnexpectedEOF
	}
	value := r.data[r.offset : r.offset+size]
	r.offset += size
	return value, nil
}
func (r *reader) skip(size int) error { _, err := r.take(size); return err }
func (r *reader) u1() (byte, error) {
	value, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}
func (r *reader) u2() (uint16, error) {
	value, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(value), nil
}
func (r *reader) u4() (uint32, error) {
	value, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(value), nil
}
