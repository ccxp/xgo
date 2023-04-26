// Code generated by protoc-gen-gogo. DO NOT EDIT.
// source: xmqinner.proto

/*
	Package xmqproto is a generated protocol buffer package.

	It is generated from these files:
		xmqinner.proto

	It has these top-level messages:
		QueueItemLog
*/
package xmqproto

import proto "github.com/golang/protobuf/proto"
import fmt "fmt"
import math "math"

import io "io"

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

// This is a compile-time assertion to ensure that this generated file
// is compatible with the proto package it is being compiled against.
// A compilation error at this line likely means your copy of the
// proto package needs to be updated.
const _ = proto.ProtoPackageIsVersion2 // please upgrade the proto package

// 记录处理状态
type QueueItemLog struct {
	Uuid1      uint64 `protobuf:"varint,1,opt,name=Uuid1,proto3" json:"Uuid1,omitempty"`
	Uuid2      uint64 `protobuf:"varint,3,opt,name=Uuid2,proto3" json:"Uuid2,omitempty"`
	UpdateTime int64  `protobuf:"varint,2,opt,name=UpdateTime,proto3" json:"UpdateTime,omitempty"`
}

func (m *QueueItemLog) Reset()                    { *m = QueueItemLog{} }
func (m *QueueItemLog) String() string            { return proto.CompactTextString(m) }
func (*QueueItemLog) ProtoMessage()               {}
func (*QueueItemLog) Descriptor() ([]byte, []int) { return fileDescriptorXmqinner, []int{0} }

func (m *QueueItemLog) GetUuid1() uint64 {
	if m != nil {
		return m.Uuid1
	}
	return 0
}

func (m *QueueItemLog) GetUuid2() uint64 {
	if m != nil {
		return m.Uuid2
	}
	return 0
}

func (m *QueueItemLog) GetUpdateTime() int64 {
	if m != nil {
		return m.UpdateTime
	}
	return 0
}

func init() {
	proto.RegisterType((*QueueItemLog)(nil), "xmqproto.QueueItemLog")
}
func (m *QueueItemLog) Marshal() (dAtA []byte, err error) {
	size := m.Size()
	dAtA = make([]byte, size)
	n, err := m.MarshalTo(dAtA)
	if err != nil {
		return nil, err
	}
	return dAtA[:n], nil
}

func (m *QueueItemLog) MarshalTo(dAtA []byte) (int, error) {
	var i int
	_ = i
	var l int
	_ = l
	if m.Uuid1 != 0 {
		dAtA[i] = 0x8
		i++
		i = encodeVarintXmqinner(dAtA, i, uint64(m.Uuid1))
	}
	if m.UpdateTime != 0 {
		dAtA[i] = 0x10
		i++
		i = encodeVarintXmqinner(dAtA, i, uint64(m.UpdateTime))
	}
	if m.Uuid2 != 0 {
		dAtA[i] = 0x18
		i++
		i = encodeVarintXmqinner(dAtA, i, uint64(m.Uuid2))
	}
	return i, nil
}

func encodeVarintXmqinner(dAtA []byte, offset int, v uint64) int {
	for v >= 1<<7 {
		dAtA[offset] = uint8(v&0x7f | 0x80)
		v >>= 7
		offset++
	}
	dAtA[offset] = uint8(v)
	return offset + 1
}
func (m *QueueItemLog) Size() (n int) {
	var l int
	_ = l
	if m.Uuid1 != 0 {
		n += 1 + sovXmqinner(uint64(m.Uuid1))
	}
	if m.UpdateTime != 0 {
		n += 1 + sovXmqinner(uint64(m.UpdateTime))
	}
	if m.Uuid2 != 0 {
		n += 1 + sovXmqinner(uint64(m.Uuid2))
	}
	return n
}

func sovXmqinner(x uint64) (n int) {
	for {
		n++
		x >>= 7
		if x == 0 {
			break
		}
	}
	return n
}
func sozXmqinner(x uint64) (n int) {
	return sovXmqinner(uint64((x << 1) ^ uint64((int64(x) >> 63))))
}
func (m *QueueItemLog) Unmarshal(dAtA []byte) error {
	l := len(dAtA)
	iNdEx := 0
	for iNdEx < l {
		preIndex := iNdEx
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return ErrIntOverflowXmqinner
			}
			if iNdEx >= l {
				return io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= (uint64(b) & 0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		fieldNum := int32(wire >> 3)
		wireType := int(wire & 0x7)
		if wireType == 4 {
			return fmt.Errorf("proto: QueueItemLog: wiretype end group for non-group")
		}
		if fieldNum <= 0 {
			return fmt.Errorf("proto: QueueItemLog: illegal tag %d (wire type %d)", fieldNum, wire)
		}
		switch fieldNum {
		case 1:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field Uuid1", wireType)
			}
			m.Uuid1 = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowXmqinner
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.Uuid1 |= (uint64(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 2:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field UpdateTime", wireType)
			}
			m.UpdateTime = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowXmqinner
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.UpdateTime |= (int64(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 3:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field Uuid2", wireType)
			}
			m.Uuid2 = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowXmqinner
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.Uuid2 |= (uint64(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		default:
			iNdEx = preIndex
			skippy, err := skipXmqinner(dAtA[iNdEx:])
			if err != nil {
				return err
			}
			if skippy < 0 {
				return ErrInvalidLengthXmqinner
			}
			if (iNdEx + skippy) > l {
				return io.ErrUnexpectedEOF
			}
			iNdEx += skippy
		}
	}

	if iNdEx > l {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func skipXmqinner(dAtA []byte) (n int, err error) {
	l := len(dAtA)
	iNdEx := 0
	for iNdEx < l {
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return 0, ErrIntOverflowXmqinner
			}
			if iNdEx >= l {
				return 0, io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= (uint64(b) & 0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		wireType := int(wire & 0x7)
		switch wireType {
		case 0:
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return 0, ErrIntOverflowXmqinner
				}
				if iNdEx >= l {
					return 0, io.ErrUnexpectedEOF
				}
				iNdEx++
				if dAtA[iNdEx-1] < 0x80 {
					break
				}
			}
			return iNdEx, nil
		case 1:
			iNdEx += 8
			return iNdEx, nil
		case 2:
			var length int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return 0, ErrIntOverflowXmqinner
				}
				if iNdEx >= l {
					return 0, io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				length |= (int(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			iNdEx += length
			if length < 0 {
				return 0, ErrInvalidLengthXmqinner
			}
			return iNdEx, nil
		case 3:
			for {
				var innerWire uint64
				var start int = iNdEx
				for shift := uint(0); ; shift += 7 {
					if shift >= 64 {
						return 0, ErrIntOverflowXmqinner
					}
					if iNdEx >= l {
						return 0, io.ErrUnexpectedEOF
					}
					b := dAtA[iNdEx]
					iNdEx++
					innerWire |= (uint64(b) & 0x7F) << shift
					if b < 0x80 {
						break
					}
				}
				innerWireType := int(innerWire & 0x7)
				if innerWireType == 4 {
					break
				}
				next, err := skipXmqinner(dAtA[start:])
				if err != nil {
					return 0, err
				}
				iNdEx = start + next
			}
			return iNdEx, nil
		case 4:
			return iNdEx, nil
		case 5:
			iNdEx += 4
			return iNdEx, nil
		default:
			return 0, fmt.Errorf("proto: illegal wireType %d", wireType)
		}
	}
	panic("unreachable")
}

var (
	ErrInvalidLengthXmqinner = fmt.Errorf("proto: negative length found during unmarshaling")
	ErrIntOverflowXmqinner   = fmt.Errorf("proto: integer overflow")
)

func init() { proto.RegisterFile("xmqinner.proto", fileDescriptorXmqinner) }

var fileDescriptorXmqinner = []byte{
	// 136 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0xe2, 0xe2, 0xab, 0xc8, 0x2d, 0xcc,
	0xcc, 0xcb, 0x4b, 0x2d, 0xd2, 0x2b, 0x28, 0xca, 0x2f, 0xc9, 0x17, 0xe2, 0xa8, 0xc8, 0x2d, 0x04,
	0xb3, 0x94, 0xa2, 0xb8, 0x78, 0x02, 0x4b, 0x53, 0x4b, 0x53, 0x3d, 0x4b, 0x52, 0x73, 0x7d, 0xf2,
	0xd3, 0x85, 0x44, 0xb8, 0x58, 0x43, 0x4b, 0x33, 0x53, 0x0c, 0x25, 0x18, 0x15, 0x18, 0x35, 0x58,
	0x82, 0x20, 0x1c, 0x21, 0x39, 0x2e, 0xae, 0xd0, 0x82, 0x94, 0xc4, 0x92, 0xd4, 0x90, 0xcc, 0xdc,
	0x54, 0x09, 0x26, 0x05, 0x46, 0x0d, 0xe6, 0x20, 0x24, 0x11, 0x98, 0x2e, 0x23, 0x09, 0x66, 0x84,
	0x2e, 0x23, 0x27, 0x81, 0x13, 0x8f, 0xe4, 0x18, 0x2f, 0x3c, 0x92, 0x63, 0x7c, 0xf0, 0x48, 0x8e,
	0x71, 0xc6, 0x63, 0x39, 0x86, 0x24, 0x36, 0xb0, 0xa5, 0xc6, 0x80, 0x00, 0x00, 0x00, 0xff, 0xff,
	0xfb, 0xe0, 0xab, 0x2f, 0x90, 0x00, 0x00, 0x00,
}
