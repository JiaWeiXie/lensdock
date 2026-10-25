// Offline font bundle. See public/licenses/NOTICE.txt for licenses (OFL-1.1).
// Latin UI and data faces.
import '@fontsource/ibm-plex-sans/latin-400.css';
import '@fontsource/ibm-plex-sans/latin-500.css';
import '@fontsource/ibm-plex-sans/latin-600.css';
import '@fontsource/ibm-plex-sans/latin-ext-400.css';
import '@fontsource/ibm-plex-sans/latin-ext-500.css';
import '@fontsource/ibm-plex-sans/latin-ext-600.css';
import '@fontsource/ibm-plex-mono/latin-400.css';
import '@fontsource/ibm-plex-mono/latin-500.css';
import '@fontsource/ibm-plex-mono/latin-ext-400.css';
// Traditional Chinese is the default; SC/JP/KR cover annotation text written
// in those languages. All are unicode-range subsets, so only the chunks a page
// actually uses are fetched.
import '@fontsource/noto-sans-tc/400.css';
import '@fontsource/noto-sans-tc/700.css';
import '@fontsource/noto-sans-sc/400.css';
import '@fontsource/noto-sans-jp/400.css';
import '@fontsource/noto-sans-kr/400.css';
// Color emoji MUST be the single full face (emoji.css). The unicode-range chunks in 400.css render
// nothing on a 2D canvas (verified in Chrome: advances are reserved but no glyph is painted, so
// exported annotation PNGs silently lost every emoji), and ZWJ sequences such as the woman
// technologist are split across chunks. The full face has no unicode-range, so the browser only
// fetches it when a character falls through the earlier families.
import '@fontsource/noto-color-emoji/emoji.css';
