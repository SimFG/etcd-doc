// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package wal

import (
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"sync"

	"go.etcd.io/etcd/client/pkg/v3/fileutil"
	"go.etcd.io/etcd/pkg/v3/crc"
	"go.etcd.io/etcd/pkg/v3/pbutil"
	"go.etcd.io/etcd/raft/v3/raftpb"
	"go.etcd.io/etcd/server/v3/storage/wal/walpb"
)

const minSectorSize = 512

// frameSizeBytes is frame size in bytes, including record size and padding size.
const frameSizeBytes = 8

type decoder struct {
	mu  sync.Mutex
	brs []*fileutil.FileBufReader

	// lastValidOff file offset following the last valid decoded record
	lastValidOff int64
	crc          hash.Hash32
}

func newDecoder(r ...fileutil.FileReader) *decoder {
	readers := make([]*fileutil.FileBufReader, len(r))
	for i := range r {
		readers[i] = fileutil.NewFileBufReader(r[i])
	}
	return &decoder{
		brs: readers,
		crc: crc.New(0, crcTable),
	}
}

func (d *decoder) decode(rec *walpb.Record) error {
	rec.Reset()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.decodeRecord(rec)
}

/***
从decoder的reader列表中读取数据到输入参数record中
- 从reader列表中第一个获取数据长度位置信息，如果失败，从下一个reader在进行尝试
- 获取数据大小和偏移
- 根据数据长度位置信息，从reader读取数据
- 解析成record
- 如果不是cyc类型数据，则进行数据校验
*/
func (d *decoder) decodeRecord(rec *walpb.Record) error {
	if len(d.brs) == 0 {
		return io.EOF
	}

	fileBufReader := d.brs[0]
	l, err := readInt64(fileBufReader)
	if err == io.EOF || (err == nil && l == 0) {
		// hit end of file or preallocated space
		d.brs = d.brs[1:]
		if len(d.brs) == 0 {
			return io.EOF
		}
		d.lastValidOff = 0
		return d.decodeRecord(rec)
	}
	if err != nil {
		return err
	}

	recBytes, padBytes := decodeFrameSize(l)
	// The length of current WAL entry must be less than the remaining file size.
	maxEntryLimit := fileBufReader.FileInfo().Size() - d.lastValidOff - padBytes
	if recBytes > maxEntryLimit {
		return fmt.Errorf("wal: max entry size limit exceeded, recBytes: %d, fileSize(%d) - offset(%d) - padBytes(%d) = entryLimit(%d)",
			recBytes, fileBufReader.FileInfo().Size(), d.lastValidOff, padBytes, maxEntryLimit)
	}

	data := make([]byte, recBytes+padBytes)
	if _, err = io.ReadFull(fileBufReader, data); err != nil {
		// ReadFull returns io.EOF only if no bytes were read
		// the decoder should treat this as an ErrUnexpectedEOF instead.
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if err := rec.Unmarshal(data[:recBytes]); err != nil {
		if d.isTornEntry(data) {
			return io.ErrUnexpectedEOF
		}
		return err
	}

	// skip crc checking if the record type is crcType
	if rec.Type != crcType {
		d.crc.Write(rec.Data)
		if err := rec.Validate(d.crc.Sum32()); err != nil {
			if d.isTornEntry(data) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
	// record decoded as valid; point last valid offset to end of record
	d.lastValidOff += frameSizeBytes + recBytes + padBytes
	return nil
}

/***
获取int64中端数据大小（低56位）和数据偏移（低56位前三位）
*/
func decodeFrameSize(lenField int64) (recBytes int64, padBytes int64) {
	// the record size is stored in the lower 56 bits of the 64-bit length
	recBytes = int64(uint64(lenField) & ^(uint64(0xff) << 56))
	// non-zero padding is indicated by set MSb / a negative length
	if lenField < 0 {
		// padding is stored in lower 3 bits of length MSB
		padBytes = int64((uint64(lenField) >> 56) & 0x7)
	}
	return recBytes, padBytes
}

// isTornEntry determines whether the last entry of the WAL was partially written
// and corrupted because of a torn write.
/***
数据是否损坏
- 将获得数据，分成多个chunk，放入chunks
- 遍历chunks，如果存在一个chunk全部是0，则数据被损坏
*/
func (d *decoder) isTornEntry(data []byte) bool {
	if len(d.brs) != 1 {
		return false
	}

	fileOff := d.lastValidOff + frameSizeBytes
	curOff := 0
	chunks := [][]byte{}
	// split data on sector boundaries
	for curOff < len(data) {
		chunkLen := int(minSectorSize - (fileOff % minSectorSize))
		if chunkLen > len(data)-curOff {
			chunkLen = len(data) - curOff
		}
		chunks = append(chunks, data[curOff:curOff+chunkLen])
		fileOff += int64(chunkLen)
		curOff += chunkLen
	}

	// if any data for a sector chunk is all 0, it's a torn write
	for _, sect := range chunks {
		isZero := true
		for _, v := range sect {
			if v != 0 {
				isZero = false
				break
			}
		}
		if isZero {
			return true
		}
	}
	return false
}

/***
更新数据crc（数据校验和）
*/
func (d *decoder) updateCRC(prevCrc uint32) {
	d.crc = crc.New(prevCrc, crcTable)
}

/***
获取数据校验和
*/
func (d *decoder) lastCRC() uint32 {
	return d.crc.Sum32()
}

/***
当前数据偏移
*/
func (d *decoder) lastOffset() int64 { return d.lastValidOff }

/***
将byte转换为raftpb.Entry
*/
func mustUnmarshalEntry(d []byte) raftpb.Entry {
	var e raftpb.Entry
	pbutil.MustUnmarshal(&e, d)
	return e
}

/***
将byte转换为raftpb.HardState
*/
func mustUnmarshalState(d []byte) raftpb.HardState {
	var s raftpb.HardState
	pbutil.MustUnmarshal(&s, d)
	return s
}

/*** 从r中读取int数值
binary包：https://www.jianshu.com/p/ec461b39bf43
大端序与小端序 https://zhuanlan.zhihu.com/p/352145413
*/
func readInt64(r io.Reader) (int64, error) {
	var n int64
	err := binary.Read(r, binary.LittleEndian, &n)
	return n, err
}
