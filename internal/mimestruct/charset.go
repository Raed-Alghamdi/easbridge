package mimestruct

// Importing the package registers go-message's charset table (message.CharsetReader):
// text parts are decoded through go-message when a SEARCH looks into them (MatchText)
// and when BINARY is asked for, and without the table a part in windows-1256 or
// ISO-8859-6 would be searched as the bytes it is. Header values are decoded by
// mimeutil.DecodeHeader, which has its own reader from the same package.
import _ "github.com/emersion/go-message/charset"
