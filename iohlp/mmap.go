/*
Copyright 2014 Tamás Gulácsi

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

package iohlp

import (
	"os"

	"github.com/edsrzf/mmap-go"
)

// MmapFile returns the mmap of the given path.
func MmapFile(fn string) ([]byte, func(), error) {
	f, err := os.Open(fn)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return Mmap(f)
}

func Mmap(f *os.File) ([]byte, func(),  error) {
	p, err := mmap.Map(f, mmap.RDONLY, 0)
	if err != nil {
		return p, nil, err
	}
	return p, func() { p.Unmap() }, nil
}
