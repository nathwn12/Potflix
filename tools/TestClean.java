import me.potflix.rpc.Main;
import java.lang.reflect.Method;

public class TestClean {
    public static void main(String[] a) throws Exception {
        String[] samples = {
            "dont_scream_together_1.mp4 - PotPlayer",
            "Breaking.Bad.S01E02.1080p.WEB-DL.DD5.1.x264-CtrlHD.mkv",
            "Dune.Part.Two.2024.2160p.WEB-DL.HDR.DDP5.1.Atmos.x265.mkv",
            "The.Office.US.S05E14.720p.HDTV.x264.mp4",
            "one_piece_ep_1071_1080p.mp4",
            "My.Video.Essay.2023.1080p.WEBRip.x264.mkv",
            "PotPlayer",
            "Breaking Bad (2008) - S02E08 - Better Call Saul (1080p BluRay x265 Silence).mkv",
            "Breaking.Bad.S02E08.Better.Call.Saul.1080p.BluRay.x265-CtrlHD.mkv",
        };
        Method clean = Class.forName("me.potflix.rpc.Main").getDeclaredMethod("clean", String.class);
        clean.setAccessible(true);
        for (String s : samples) {
            String raw = s.endsWith(" - PotPlayer") ? s.substring(0, s.lastIndexOf(" - PotPlayer")) : s;
            Object c = clean.invoke(null, raw);
            var title = c.getClass().getDeclaredField("title"); title.setAccessible(true);
            var display = c.getClass().getDeclaredField("display"); display.setAccessible(true);
            var ep = c.getClass().getDeclaredField("episodeTag"); ep.setAccessible(true);
            System.out.println("IN : " + s);
            System.out.println("  title=[" + title.get(c) + "] ep=[" + ep.get(c) + "] display=[" + display.get(c) + "]");
        }
    }
}
