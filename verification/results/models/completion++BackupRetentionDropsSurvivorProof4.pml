// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go#L4
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_BackupRetentionDropsSurvivorProof40 = [1] of {int}->
	run BackupRetentionDropsSurvivorProof4(child_BackupRetentionDropsSurvivorProof40)->
	child_BackupRetentionDropsSurvivorProof40?0->
stop_process:skip
}

proctype BackupRetentionDropsSurvivorProof4(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousBackupRetentionDropsSurvivorProof1052 = [1] of {int}->
	chan child_AnonymousBackupRetentionDropsSurvivorProof961 = [1] of {int}->
	chan child_AnonymousBackupRetentionDropsSurvivorProof850 = [1] of {int}->
	Chandef removed_ch->
	Chandef unchanged_ch->
	Chandef survivor_ch->
	run sync_monitor(survivor_ch)->
	run sync_monitor(unchanged_ch)->
	run sync_monitor(removed_ch)->
	run AnonymousBackupRetentionDropsSurvivorProof85(survivor_ch,unchanged_ch,removed_ch,child_AnonymousBackupRetentionDropsSurvivorProof850)->
	run receiver(child_AnonymousBackupRetentionDropsSurvivorProof850)->
	run AnonymousBackupRetentionDropsSurvivorProof96(unchanged_ch,removed_ch,survivor_ch,child_AnonymousBackupRetentionDropsSurvivorProof961)->
	run receiver(child_AnonymousBackupRetentionDropsSurvivorProof961)->
	run AnonymousBackupRetentionDropsSurvivorProof105(survivor_ch,unchanged_ch,removed_ch,child_AnonymousBackupRetentionDropsSurvivorProof1052)->
	run receiver(child_AnonymousBackupRetentionDropsSurvivorProof1052)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:11:2 */
	:: removed_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:11:2 */->
	:: removed_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:11:2 */ -> 
		removed_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupRetentionDropsSurvivorProof85(Chandef survivor_ch;Chandef unchanged_ch;Chandef removed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:8:12 */
	:: survivor_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:8:12 */
	:: survivor_ch.sync!false -> 
		survivor_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:8:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupRetentionDropsSurvivorProof96(Chandef unchanged_ch;Chandef removed_ch;Chandef survivor_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:9:12 */
	:: unchanged_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:9:12 */
	:: unchanged_ch.sync!false -> 
		unchanged_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:9:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupRetentionDropsSurvivorProof105(Chandef survivor_ch;Chandef unchanged_ch;Chandef removed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:10:12 */
	:: unchanged_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:10:12 */->
	:: unchanged_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:10:12 */ -> 
		unchanged_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:10:25 */
	:: removed_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:10:25 */
	:: removed_ch.sync!false -> 
		removed_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_retention.go:10:25 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

