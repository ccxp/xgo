package protoutil

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unsafe"
)

const (
	WireVarint     = 0
	WireFixed64    = 1
	WireBytes      = 2
	WireStartGroup = 3
	WireEndGroup   = 4
	WireFixed32    = 5
)

var (
	ErrInvalidLength = fmt.Errorf("proto: negative length found during unmarshaling")
	ErrIntOverflow   = fmt.Errorf("proto: integer overflow")
)

// 合并wireType和field id
func WireEncode(wireType, fieldId int) uint64 {
	return uint64(fieldId)<<3 | uint64(wireType)
}

// 拆分wireType和field id
func WireDecode(x uint64) (wireType, fieldId int) {
	return int(x & 0x7), int(x >> 3)
}

// 取变长整数编码后大小. 注意sint32、sint64应该使用SizeOfSint
func SizeOfVarint(x uint64) (n int) {
	for {
		n++
		x >>= 7
		if x == 0 {
			break
		}
	}
	return n
}

// 取得sint32、sint64编码后大小. 传参直接用uint64(v)
func SizeOfSint(x uint64) (n int) {
	v := uint64((x << 1) ^ uint64((int64(x) >> 63)))
	return SizeOfVarint(v)
}

// 把uint64以变长方式写进buffer. 注意offset是已经写完wire+tag后的数据位置.
func VarintEncode(data []byte, offset int, v uint64) int {
	for v >= 1<<7 {
		data[offset] = uint8(v&0x7f | 0x80)
		v >>= 7
		offset++
	}
	data[offset] = uint8(v)
	return offset + 1
}

// 从buffer读取变长保存的数据，并且返回数据结束位置. 注意offset是已经写完wire+tag后的数据位置.
func VarintDecode(data []byte, offset int) (v uint64, newOffset int, err error) {

	l := len(data)
	var b byte
	for shift := uint(0); ; shift += 7 {
		if shift >= 64 {
			return v, offset, ErrIntOverflow
		}
		if offset >= l {
			return v, offset, io.ErrUnexpectedEOF
		}
		b = data[offset]
		offset++
		v |= (uint64(b) & 0x7F) << shift
		if b < 0x80 {
			break
		}
	}
	return v, offset, nil
}

// sint32、sint64以变长方式写进buffer. 注意offset是已经写完wire+tag后的数据位置.
func SintEncode(data []byte, offset int, x uint64) int {
	v := uint64((x << 1) ^ uint64((int64(x) >> 63)))
	return VarintEncode(data, offset, v)
}

// 从buffer读取变长保存的sint32，并且返回数据结束位置. 注意offset是已经写完wire+tag后的数据位置.
func Sint32Decode(data []byte, offset int) (v int32, newOffset int, err error) {
	x, newOffset, err := VarintDecode(data, offset)
	v = int32((uint32(x) >> 1) ^ uint32((int32(x&1)<<31)>>31))
	return
}

// 从buffer读取变长保存的sint64，并且返回数据结束位置. 注意offset是已经写完wire+tag后的数据位置.
func Sint64Decode(data []byte, offset int) (v int64, newOffset int, err error) {
	x, newOffset, err := VarintDecode(data, offset)
	v = int64((x >> 1) ^ uint64((int64(x&1)<<63)>>63))
	return
}

// 把定长4节写进buffer. 注意offset是已经写完wire+tag后的数据位置.
func Fixed32Encode(data []byte, offset int, v unsafe.Pointer) int {
	binary.LittleEndian.PutUint32(data[offset:], *(*uint32)(v))
	return offset + 4
}

// 从buffer读取定长4节数据，并且返回数据结束位置. 注意offset是已经写完wire+tag后的数据位置.
func Fixed32Decode(data []byte, offset int, v unsafe.Pointer) (newOffset int, err error) {
	*(*uint32)(v) = binary.LittleEndian.Uint32(data[offset:])
	return offset + 4, nil
}

// 把定长8节写进buffer. 注意offset是已经写完wire+tag后的数据位置.
func Fixed64Encode(data []byte, offset int, v unsafe.Pointer) int {
	binary.LittleEndian.PutUint64(data[offset:], *(*uint64)(v))
	return offset + 8
}

// 从buffer读取定长8节数据，并且返回数据结束位置. 注意offset是已经写完wire+tag后的数据位置.
func Fixed64Decode(data []byte, offset int, v unsafe.Pointer) (newOffset int, err error) {
	*(*uint64)(v) = binary.LittleEndian.Uint64(data[offset:])
	return offset + 8, nil
}

// 当遇到未定义的项时，可用Skip跳到下个数据. 注意数据是不包含wire+tag的数据.
func Skip(data []byte, offset, wireType int) (newOffset int, err error) {
	l := len(data)

	switch wireType {
	case WireVarint:
		_, newOffset, err = VarintDecode(data, offset)
		return
	case WireFixed64:
		if offset+8 > l {
			return offset, io.ErrUnexpectedEOF
		}
		return offset + 8, nil
	case WireFixed32:
		if offset+4 > l {
			return offset, io.ErrUnexpectedEOF
		}
		return offset + 4, nil
	case WireBytes:
		var length uint64
		length, newOffset, err = VarintDecode(data, offset)
		if err != nil {
			return offset, err
		}
		newOffset += int(length)
		if newOffset > l {
			return 0, ErrInvalidLength
		}
		return newOffset, nil
	case WireStartGroup:
		var innerWire uint64
		var innerWireType int
		for {
			innerWire, newOffset, err = VarintDecode(data, offset)
			if err != nil {
				return
			}
			innerWireType, _ = WireDecode(innerWire)
			if innerWireType == WireEndGroup {
				return
			}
			newOffset, err = Skip(data, newOffset, innerWireType)
			if err != nil {
				return
			}
		}
	case WireEndGroup:
		return offset, nil

	default:
		return 0, fmt.Errorf("proto: illegal wireType %d", wireType)
	}
}

/*
// 复制string进buffer，不用[]byte(s)少一次数据复制
func PutString(data []byte, s string) {
	sh := (*reflect.StringHeader)(unsafe.Pointer(&s))
	copy(data, *(*[]byte)(unsafe.Pointer(&reflect.SliceHeader{Data: sh.Data, Len: sh.Len, Cap: sh.Len})))
}*/

// 变长整数加上wireType、fieldId后的长度
func VarintDataSize(fieldId int, v uint64) int {
	return SizeOfVarint(WireEncode(WireVarint, fieldId)) + SizeOfVarint(v)
}

// sint32、sint64加上wireType、fieldId后的长度
func SintDataSize(fieldId int, v uint64) int {
	return SizeOfVarint(WireEncode(WireVarint, fieldId)) + SizeOfSint(v)
}

// 定长32位加上wireType、fieldId后的长度
func Fixed32DataSize(fieldId int) int {
	return SizeOfVarint(WireEncode(WireFixed32, fieldId)) + 4
}

// 定长64位加上wireType、fieldId后的长度
func Fixed64DataSize(fieldId int) int {
	return SizeOfVarint(WireEncode(WireFixed64, fieldId)) + 8
}

// bytes、string、message类型加上wireType、fieldId后的长度.
func BytesDataSize(fieldId int, byteSize int) int {
	return SizeOfVarint(WireEncode(WireBytes, fieldId)) + SizeOfVarint(uint64(byteSize)) + byteSize
}

func Uint64ToSint32(x uint64) int32 {
	return int32((uint32(x) >> 1) ^ uint32((int32(x&1)<<31)>>31))
}

func Uint64ToSint64(x uint64) int64 {
	return int64((x >> 1) ^ uint64((int64(x&1)<<63)>>63))
}

// 流式处理protobuf用.
//
// 先用ReadWire得到 wireType, fieldNum，再根据wireType读不同的值.
// 如果是 WireBytes 这种类型，有可能是个message，请根据fieldNum确定.
//
// 注意在递归处理message时，
// 需要先读length，再用 LimitReader 限制里面的长度.
type Reader struct {
	io.Reader
}

func (r *Reader) ReadVarint() (v uint64, e error) {

	b := make([]byte, 1)
	for shift := uint(0); ; shift += 7 {
		if shift >= 64 {
			return v, ErrIntOverflow
		}
		_, e = r.ReadFull(b)
		if e != nil {
			return v, e
		}

		v |= (uint64(b[0]) & 0x7F) << shift
		if b[0] < 0x80 {
			break
		}
	}

	return v, nil
}

// 注意返回 io.Eof 代表没有数据了
func (r *Reader) ReadWire() (wireType, fieldNum int, e error) {
	v, e := r.ReadVarint()
	if e != nil {
		return
	}
	wireType, fieldNum = WireDecode(v)
	return
}

func (r *Reader) ReadFixed64() (f float64, e error) {
	b := make([]byte, 8)
	_, e = r.ReadFull(b)
	if e != nil {
		return
	}

	v := binary.LittleEndian.Uint64(b)
	f = math.Float64frombits(v)
	return
}

func (r *Reader) ReadFixed32() (f float32, e error) {
	b := make([]byte, 4)
	_, e = r.ReadFull(b)
	if e != nil {
		return
	}

	v := binary.LittleEndian.Uint32(b)
	f = math.Float32frombits(v)
	return
}

func (r *Reader) ReadFull(buf []byte) (int, error) {
	n, e := io.ReadFull(r, buf)
	if e == io.EOF {
		e = io.ErrUnexpectedEOF
	}
	return n, e
}
