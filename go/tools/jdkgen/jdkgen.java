import java.io.*;
import java.util.*;

// Emits jdk11tables.go: JDK-11 (Unicode 10) exact word / non-spacing-mark /
// simple-upper / full-upper tables derived from the installed JDK.
class jdkgen {
    static StringBuilder sb = new StringBuilder();
    static final int MAX = 0x10FFFF;

    static void emitRanges2(String name, List<int[]> rs) {
        sb.append("var ").append(name).append(" = [][2]rune{\n");
        int col = 0;
        for (int[] r : rs) {
            sb.append(String.format("{0x%X,0x%X},", r[0], r[1]));
            if (++col % 14 == 0) sb.append('\n');
        }
        sb.append("\n}\n\n");
    }

    static void emitRanges3(String name, List<int[]> rs) {
        sb.append("var ").append(name).append(" = [][3]rune{\n");
        int col = 0;
        for (int[] r : rs) {
            sb.append(String.format("{0x%X,0x%X,%d},", r[0], r[1], r[2]));
            if (++col % 10 == 0) sb.append('\n');
        }
        sb.append("\n}\n\n");
    }

    public static void main(String[] a) throws Exception {
        Locale.setDefault(Locale.US);

        List<int[]> word = new ArrayList<>();
        for (int cp = 0; cp <= MAX; cp++) {
            if (!Character.isLetterOrDigit(cp)) continue;
            int lo = cp;
            while (cp < MAX && Character.isLetterOrDigit(cp + 1)) cp++;
            word.add(new int[]{lo, cp});
        }

        List<int[]> mn = new ArrayList<>();
        for (int cp = 0; cp <= MAX; cp++) {
            if (Character.getType(cp) != Character.NON_SPACING_MARK) continue;
            int lo = cp;
            while (cp < MAX && Character.getType(cp + 1) == Character.NON_SPACING_MARK) cp++;
            mn.add(new int[]{lo, cp});
        }

        List<int[]> upper = new ArrayList<>();
        for (int cp = 0; cp <= 0xFFFF; cp++) {
            if (cp >= 0xD800 && cp <= 0xDFFF) continue;
            int u = Character.toUpperCase((char) cp);
            if (u == cp) continue;
            int lo = cp, hi = cp, delta = u - cp;
            while (hi < 0xFFFF) {
                int n = hi + 1;
                if (n >= 0xD800 && n <= 0xDFFF) break;
                int nu = Character.toUpperCase((char) n);
                if (nu == n || nu - n != delta) break;
                hi = n;
            }
            cp = hi;
            upper.add(new int[]{lo, hi, delta});
        }

        List<String[]> full = new ArrayList<>();
        for (int cp = 0; cp <= 0xFFFF; cp++) {
            if (cp >= 0xD800 && cp <= 0xDFFF) continue;
            String s = new String(Character.toChars(cp));
            String fu = s.toUpperCase(Locale.US);
            String simple = String.valueOf(Character.toUpperCase((char) cp));
            if (!fu.equals(simple)) full.add(new String[]{String.format("0x%04X", cp), goStr(fu)});
        }

        sb.append("package titleclean\n\n");
        sb.append("// Code generated from the installed JDK 11 (Unicode 10) by jdkgen; DO NOT EDIT.\n");
        sb.append("// isLetterOrDigit / NON_SPACING_MARK / Character.toUpperCase / full toUpperCase.\n\n");
        emitRanges2("jdkWordRanges", word);
        emitRanges2("jdkMnRanges", mn);
        emitRanges3("jdkUpperRanges", upper);
        sb.append("var jdkFullUpper = map[rune]string{\n");
        for (String[] f : full) sb.append(f[0]).append(": ").append(f[1]).append(",\n");
        sb.append("}\n");

        try (Writer w = new OutputStreamWriter(new FileOutputStream("jdk11tables.go"), "UTF-8")) {
            w.write(sb.toString());
        }
        System.out.println("wordRanges=" + word.size() + " mnRanges=" + mn.size()
            + " upperRanges=" + upper.size() + " fullUpper=" + full.size());
    }

    static String goStr(String s) {
        StringBuilder b = new StringBuilder("\"");
        for (int i = 0; i < s.length(); i++) b.append(String.format("\\u%04X", (int) s.charAt(i)));
        return b.append('"').toString();
    }
}
