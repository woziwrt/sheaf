# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Summary of a 2-link sheaf SENDER trace (-trace, 100 ms rows), rows with traffic only:
# per-link tx rate, mean window / in flight / srtt, how often the window was full, how often it was small.
# Usage: awk -f trace-summary.awk st-down-...-B.csv
BEGIN{FS=","}
NR==1{next}
{ tx=$6+$13; if(tx==0 && !act) next; act=1; if(tx==0){idle++; next}
  n++; for(k=0;k<2;k++){o=1+7*k; w[k]+=$(o+1); f[k]+=$(o+2); s[k]+=$(o+3); d[k]+=$(o+4); t[k]+=$(o+5); td[k]+=$(o+6);
     if($(o+2)>=0.9*$(o+1)) full[k]++; if($(o+3)>mx[k]) mx[k]=$(o+3); if(mn[k]==""||($(o+3)>0&&$(o+3)<mn[k])) mn[k]=$(o+3);
     if($(o+1)<100) small[k]++ } }
END{ if(n==0){ print "no rows with traffic"; exit }
     for(k=0;k<2;k++) printf "L%d: tx %.1f Mbit/s  win %.0fk infl %.0fk srtt %.0f ms (min %d max %d)  window-full %.0f%%  win<100k %.0f%%  dead-rows %d txdead %d\n",
       k+1, t[k]/n*10*1468*8/1e6, w[k]/n, f[k]/n, s[k]/n, mn[k], mx[k], 100*full[k]/n, 100*small[k]/n, d[k], td[k];
     printf "rows %d (%.1f s), idle rows inside %d\n", n, n/10, idle }
