// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go#L3
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
	chan child_BackupUnpinDropsPendingClaimCheck30 = [1] of {int}->
	run BackupUnpinDropsPendingClaimCheck3(child_BackupUnpinDropsPendingClaimCheck30)->
	child_BackupUnpinDropsPendingClaimCheck30?0->
stop_process:skip
}

proctype BackupUnpinDropsPendingClaimCheck3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousBackupUnpinDropsPendingClaimCheck942 = [1] of {int}->
	chan child_AnonymousBackupUnpinDropsPendingClaimCheck851 = [1] of {int}->
	chan child_AnonymousBackupUnpinDropsPendingClaimCheck740 = [1] of {int}->
	Chandef unpinned_ch->
	Chandef unclaimed_ch->
	Chandef proof_ch->
	run sync_monitor(proof_ch)->
	run sync_monitor(unclaimed_ch)->
	run sync_monitor(unpinned_ch)->
	run AnonymousBackupUnpinDropsPendingClaimCheck74(proof_ch,unclaimed_ch,unpinned_ch,child_AnonymousBackupUnpinDropsPendingClaimCheck740)->
	run receiver(child_AnonymousBackupUnpinDropsPendingClaimCheck740)->
	run AnonymousBackupUnpinDropsPendingClaimCheck85(unclaimed_ch,unpinned_ch,proof_ch,child_AnonymousBackupUnpinDropsPendingClaimCheck851)->
	run receiver(child_AnonymousBackupUnpinDropsPendingClaimCheck851)->
	run AnonymousBackupUnpinDropsPendingClaimCheck94(proof_ch,unclaimed_ch,unpinned_ch,child_AnonymousBackupUnpinDropsPendingClaimCheck942)->
	run receiver(child_AnonymousBackupUnpinDropsPendingClaimCheck942)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:10:2 */
	:: unpinned_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:10:2 */->
	:: unpinned_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:10:2 */ -> 
		unpinned_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupUnpinDropsPendingClaimCheck74(Chandef proof_ch;Chandef unclaimed_ch;Chandef unpinned_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:7:12 */
	:: proof_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:7:12 */
	:: proof_ch.sync!false -> 
		proof_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:7:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupUnpinDropsPendingClaimCheck85(Chandef unclaimed_ch;Chandef unpinned_ch;Chandef proof_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:8:12 */
	:: unclaimed_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:8:12 */
	:: unclaimed_ch.sync!false -> 
		unclaimed_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:8:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupUnpinDropsPendingClaimCheck94(Chandef proof_ch;Chandef unclaimed_ch;Chandef unpinned_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:9:12 */
	:: proof_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:9:12 */->
	:: proof_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:9:12 */ -> 
		proof_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:9:20 */
	:: unpinned_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:9:20 */
	:: unpinned_ch.sync!false -> 
		unpinned_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_unpin.go:9:20 */
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

