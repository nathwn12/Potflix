Add-Type -AssemblyName System.Drawing
$ErrorActionPreference = 'Stop'

function New-Icon($size, $path, $kind) {
  $bmp = New-Object System.Drawing.Bitmap($size, $size)
  $g = [System.Drawing.Graphics]::FromImage($bmp)
  $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
  $g.TextRenderingHint = [System.Drawing.Text.TextRenderingHint]::AntiAliasGridFit
  $g.Clear([System.Drawing.Color]::Transparent)
  $bg = New-Object System.Drawing.SolidBrush([System.Drawing.ColorTranslator]::FromHtml('#141414'))
  $red = New-Object System.Drawing.SolidBrush([System.Drawing.ColorTranslator]::FromHtml('#E50914'))
  $white = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::White)
  $r = [System.Drawing.Rectangle]::new(0, 0, $size, $size)
  $rad = [int]($size * 0.225)
  $gp = New-Object System.Drawing.Drawing2D.GraphicsPath
  $gp.AddArc($r.X, $r.Y, $rad*2, $rad*2, 180, 90)
  $gp.AddArc($r.Right-$rad*2, $r.Y, $rad*2, $rad*2, 270, 90)
  $gp.AddArc($r.Right-$rad*2, $r.Bottom-$rad*2, $rad*2, $rad*2, 0, 90)
  $gp.AddArc($r.X, $r.Bottom-$rad*2, $rad*2, $rad*2, 90, 90)
  $gp.CloseFigure()
  $g.FillPath($bg, $gp)
  $cx = $size / 2; $cy = $size / 2
  if ($kind -eq 'logo') {
    $fs = [int]($size * 0.62)
    $fam = New-Object System.Drawing.FontFamily('Arial Black')
    try { $fam = New-Object System.Drawing.FontFamily('Segoe UI Black') } catch {}
    $f = New-Object System.Drawing.Font($fam, $fs, [System.Drawing.FontStyle]::Bold, [System.Drawing.GraphicsUnit]::Pixel)
    $sf = New-Object System.Drawing.StringFormat
    $sf.Alignment = [System.Drawing.StringAlignment]::Center
    $sf.LineAlignment = [System.Drawing.StringAlignment]::Center
    $g.DrawString('P', $f, $red, [System.Drawing.RectangleF]::new(0, -$size*0.04, $size, $size), $sf)
    $barH = [int]($size * 0.045)
    $barW = [int]($size * 0.42)
    $g.FillRectangle($red, $cx-$barW/2, $size*0.80, $barW, $barH)
  } else {
    $d = $size * 0.62; $bx = $cx - $d/2; $by = $cy - $d/2
    $g.FillEllipse($red, $bx, $by, $d, $d)
    if ($kind -eq 'play') {
      $s = [double]$d * 0.42
      $pts = [System.Drawing.PointF[]]@(
        [System.Drawing.PointF]::new([float]($cx-$s*0.35), [float]($cy-$s*0.55)),
        [System.Drawing.PointF]::new([float]($cx-$s*0.35), [float]($cy+$s*0.55)),
        [System.Drawing.PointF]::new([float]($cx+$s*0.6), [float]$cy)
      )
      $g.FillPolygon($white, $pts)
    } elseif ($kind -eq 'pause') {
      $w = $d*0.16; $h = $d*0.44; $gap = $d*0.10
      $g.FillRectangle($white, $cx-$gap-$w, $cy-$h/2, $w, $h)
      $g.FillRectangle($white, $cx+$gap, $cy-$h/2, $w, $h)
    } else {
      $s = $d*0.40
      $g.FillRectangle($white, $cx-$s/2, $cy-$s/2, $s, $s)
    }
  }
  $g.Dispose(); $bg.Dispose(); $red.Dispose(); $white.Dispose()
  $bmp.Save($path, [System.Drawing.Imaging.ImageFormat]::Png)
  $bmp.Dispose()
  Write-Host "wrote $path"
}

$root = Split-Path $MyInvocation.MyCommand.Path -Parent | Split-Path -Parent
New-Icon 256 "$root\src\main\resources\icon.png" 'logo'
New-Icon 1024 "$root\assets\discord\logo.png" 'logo'
New-Icon 512 "$root\assets\discord\play.png" 'play'
New-Icon 512 "$root\assets\discord\pause.png" 'pause'
New-Icon 512 "$root\assets\discord\stop.png" 'stop'
