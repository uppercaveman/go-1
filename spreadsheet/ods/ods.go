/*
Copyright 2019 Tamás Gulácsi

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ods

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"hash/fnv"
	"io"
	"sync"
	"strings"
	"time"

	qt "github.com/valyala/quicktemplate"
	errors "golang.org/x/xerrors"

	"github.com/tgulacsi/go/spreadsheet"
)

var _  = errors.Errorf

//go:generate qtc

var qtMu sync.Mutex

// AcquireWriter wraps the given io.Writer to be usable with quicktemplates.
func AcquireWriter(w io.Writer) *qt.Writer {
	qtMu.Lock()
	W := qt.AcquireWriter(w)
	qtMu.Unlock()
	return W
}

// ReleaseWriter returns the *quicktemplate.Writer to the pool.
func ReleaseWriter(W *qt.Writer) { qtMu.Lock(); qt.ReleaseWriter(W); qtMu.Unlock() }

// ValueType is the cell's value's type.
type ValueType uint8

func (v ValueType) String() string {
	switch v {
	case 'f':
		return "float"
	case 'd':
		return "date"
	default:
		return "string"
	}
}
func getValueType(v interface{}) ValueType {
	switch v.(type) {
	case float32, float64,
		int, int8, int16, int32, int64,
		uint, uint16, uint32, uint64:
		return FloatType
	case time.Time:
		return DateType
	default:
		return StringType
	}
}

const (
	// FloatType for numerical data
	FloatType = ValueType('f')
	// DateType for dates
	DateType = ValueType('d')
	// StringType for everything else
	StringType = ValueType('s')
)


// NewWriter returns a content writer and a zip closer for an ods file.
func NewWriter(w io.Writer) (*ODSWriter, error) {
	zw := zip.NewWriter(w)
	for _, elt := range []struct {
		Name string
		Stream func(*qt.Writer) 
	}{
		{"mimetype", StreamMimetype},
		{"meta.xml", StreamMeta},
		{"META-INF/manifest.xml", StreamManifest},
		{"settings.xml", StreamSettings},
	} {
		parts := strings.SplitAfter(elt.Name, "/")
		var prev string
		for _, p := range parts[:len(parts)-1] {
			prev += p
			if _, err := zw.CreateHeader(&zip.FileHeader{Name:prev}); err != nil {
				return nil, err
			}
		}
		sub, err := zw.CreateHeader(&zip.FileHeader{Name:elt.Name})
		if err != nil {
			zw.Close()
			return nil, err
		}
		W := AcquireWriter(sub)
		elt.Stream(W)
		ReleaseWriter(W)
	}

	bw, err := zw.Create("content.xml")
	if err != nil {
		zw.Close()
		return nil, err
	}
	W := AcquireWriter(bw)
	StreamBeginSpreadsheet(W)

	return &ODSWriter{qtWriter: W, zipWriter: zw}, nil
}

// ODSWriter writes content.xml of ODS zip.
type ODSWriter struct {
	qtWriter  *qt.Writer
	zipWriter *zip.Writer
	styles    map[string]string
}

func (ow *ODSWriter) QTWriter() *qt.Writer { return ow.qtWriter }

// Close the ODSWriter.
func (ow *ODSWriter) Close() error {
	if ow == nil || ow.qtWriter == nil {
		return nil
	}
	StreamEndSpreadsheet(ow.qtWriter)
	ReleaseWriter(ow.qtWriter)
	ow.qtWriter = nil
	zw := ow.zipWriter
	ow.zipWriter = nil
	defer zw.Close()
	bw, err := zw.Create("styles.xml")
	if err != nil {
		return err
	}
	W := AcquireWriter(bw)
	StreamStyles(W, ow.styles)
	ReleaseWriter(W)
	return zw.Close()
}

func (ow *ODSWriter) NewSheet(name string, cols []spreadsheet.Column) (spreadsheet.Sheet, error) {
	ow.StreamBeginSheet(ow.qtWriter, name, cols)
	return &ODSSheet{ow: ow}, nil
}

func (ow *ODSWriter) getStyleName(style spreadsheet.Style) string {
	if !style.FontBold {
		return ""
	}
	hsh := fnv.New32()
	//fmt.Fprintf(hsh, "%t\t%s", style.FontBold, style.Format)
	fmt.Fprintf(hsh, "%t", style.FontBold)
	k := fmt.Sprintf("bf-%d", hsh.Sum32())
	if _, ok := ow.styles[k]; ok {
		return k
	}
	if ow.styles == nil {
		ow.styles = make(map[string]string, 1)
	}
	ow.styles[k] = `<style:style style:name="` + k + `" style:family="table-cell"><style:text-properties text:display="true" fo:font-weight="bold" /></style:style>`
	return k
}

type ODSSheet struct {
	ow *ODSWriter
}

func (ods *ODSSheet) AppendRow(values ...interface{}) error {
	StreamRow(ods.ow.qtWriter, values...)
	return nil
}

func (ods *ODSSheet) Close() error {
	ow := ods.ow
	ods.ow = nil
	if ow != nil {
		ow.StreamEndSheet(ow.qtWriter)
	}
	return nil
}

// Style information - generated from content.xml with github.com/miek/zek/cmd/zek.
type Style struct {
	XMLName         xml.Name `xml:"style"`
	Name            string   `xml:"name,attr"`
	Family          string   `xml:"family,attr"`
	MasterPageName  string   `xml:"master-page-name,attr"`
	DataStyleName   string   `xml:"data-style-name,attr"`
	TableProperties struct {
		Display     string `xml:"display,attr"`
		WritingMode string `xml:"writing-mode,attr"`
	} `xml:"table-properties"`
	TextProperties struct {
		FontWeight           string `xml:"font-weight,attr"`
		FontStyle            string `xml:"font-style,attr"`
		TextPosition         string `xml:"text-position,attr"`
		TextLineThroughType  string `xml:"text-line-through-type,attr"`
		TextLineThroughStyle string `xml:"text-line-through-style,attr"`
		TextUnderlineType    string `xml:"text-underline-type,attr"`
		TextUnderlineStyle   string `xml:"text-underline-style,attr"`
		TextUnderlineWidth   string `xml:"text-underline-width,attr"`
		Display              string `xml:"display,attr"`
		TextUnderlineColor   string `xml:"text-underline-color,attr"`
		TextUnderlineMode    string `xml:"text-underline-mode,attr"`
		FontSize             string `xml:"font-size,attr"`
		Color                string `xml:"color,attr"`
		FontFamily           string `xml:"font-family,attr"`
	} `xml:"text-properties"`
	TableRowProperties struct {
		RowHeight           string `xml:"row-height,attr"`
		UseOptimalRowHeight string `xml:"use-optimal-row-height,attr"`
	} `xml:"table-row-properties"`
	TableColumnProperties struct {
		ColumnWidth           string `xml:"column-width,attr"`
		UseOptimalColumnWidth string `xml:"use-optimal-column-width,attr"`
	} `xml:"table-column-properties"`
	TableCellProperties struct {
		BackgroundColor          string `xml:"background-color,attr"`
		BorderTop                string `xml:"border-top,attr"`
		BorderBottom             string `xml:"border-bottom,attr"`
		BorderLeft               string `xml:"border-left,attr"`
		BorderRight              string `xml:"border-right,attr"`
		DiagonalBlTr             string `xml:"diagonal-bl-tr,attr"`
		DiagonalTlBr             string `xml:"diagonal-tl-br,attr"`
		VerticalAlign            string `xml:"vertical-align,attr"`
		WrapOption               string `xml:"wrap-option,attr"`
		ShrinkToFit              string `xml:"shrink-to-fit,attr"`
		WritingMode              string `xml:"writing-mode,attr"`
		GlyphOrientationVertical string `xml:"glyph-orientation-vertical,attr"`
		CellProtect              string `xml:"cell-protect,attr"`
		RotationAlign            string `xml:"rotation-align,attr"`
		RotationAngle            string `xml:"rotation-angle,attr"`
		PrintContent             string `xml:"print-content,attr"`
		DecimalPlaces            string `xml:"decimal-places,attr"`
		TextAlignSource          string `xml:"text-align-source,attr"`
		RepeatContent            string `xml:"repeat-content,attr"`
	} `xml:"table-cell-properties"`
	ParagraphProperties struct {
		WritingModeAutomatic string `xml:"writing-mode-automatic,attr"`
		MarginLeft           string `xml:"margin-left,attr"`
	} `xml:"paragraph-properties"`
}
