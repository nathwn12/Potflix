import me.potflix.rpc.Main;
import java.lang.reflect.*;
import java.nio.file.*;
import java.util.*;

// Batch-runs the Potflix cleaner over titles-sample.txt, prints only SUSPECT outputs.
public class BatchClean {
    static int total = 0, suspect = 0;
    public static void main(String[] a) throws Exception {
        Class<?> c = Class.forName("me.potflix.rpc.Main");
        Method clean = c.getDeclaredMethod("clean", String.class);
        clean.setAccessible(true);
        Field fT = null, fD = null, fE = null;
        for (String line : Files.readAllLines(Paths.get("titles-sample.txt"), java.nio.charset.StandardCharsets.UTF_8)) {
            String raw = line.trim();
            if (raw.isEmpty()) continue;
            total++;
            Object o = clean.invoke(null, raw.replaceAll(" - PotPlayer$", ""));
            if (fT == null) {
                fT = o.getClass().getDeclaredField("title"); fT.setAccessible(true);
                fD = o.getClass().getDeclaredField("display"); fD.setAccessible(true);
                fE = o.getClass().getDeclaredField("episodeTag"); fE.setAccessible(true);
            }
            String d = (String) fD.get(o);
            List<String> why = new ArrayList<>();
            if (d.matches("(?i).*\\b(1080p?|720p?|2160p?|4k|x26[45]|xvid|hevc|av1|bluray|bdrip|web[ .\\-]?dl|webrip|hdtv|ddp?5|atmos|eac3|dts|repack|proper)\\b.*")) why.add("leftover-tag");
            if (d.matches(".*[\\[\\]].*")) why.add("brackets");
            if (d.length() > 70) why.add("too-long");
            if (d.matches(".*\\b[A-Z0-9]{5,}\\b.*") && !d.matches(".*\\b(19\\d{2}|20\\d{2})\\b.*")) why.add("caps-junk");
            if (d.matches("(?i).*\\b(s\\d{1,2}e\\d{1,2}|\\d{3,4}x\\d{3,4})\\b.*")) why.add("raw-codes");
            if (d.matches("(?i).*\\b(\\d{1,2}[-_.]\\d{1,2}[-_.]\\d{2,4})\\b.*")) why.add("raw-codes");
            if (!why.isEmpty()) {
                suspect++;
                System.out.println("IN : " + raw);
                System.out.println("  -> [" + d + "] (" + String.join(",", why) + ")");
            }
        }
        System.out.println("TOTAL=" + total + " SUSPECT=" + suspect);
    }
}
