// protobuf的一些公共数据处理.
//
// protobuf编码方式：
//
// 	整数:            VarintEncode(WireEncode(WireVarint, fieldId)) + VarintEncode(v)
// 	SINT32、SINT64:  VarintEncode(WireEncode(WireVarint, fieldId)) + VarintEncode(SintEncode(v))
// 	定长数据:         VarintEncode(WireEncode(WireFixed32 或 WireFixed64, fieldId)) + v
// 	bytes、string:   VarintEncode(WireEncode(WireBytes, fieldId)) + VarintEncode( len(b) ) + b
//
// VarintEncode保存的字节长度可用SizeOfVarint(v)获得.
//
package protoutil
