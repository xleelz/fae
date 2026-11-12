// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go#L4
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
	chan child_CleanupWaitsForBackupAndArchive40 = [1] of {int}->
	run CleanupWaitsForBackupAndArchive4(child_CleanupWaitsForBackupAndArchive40)->
	child_CleanupWaitsForBackupAndArchive40?0->
stop_process:skip
}

proctype CleanupWaitsForBackupAndArchive4(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousCleanupWaitsForBackupAndArchive1052 = [1] of {int}->
	chan child_AnonymousCleanupWaitsForBackupAndArchive951 = [1] of {int}->
	chan child_AnonymousCleanupWaitsForBackupAndArchive850 = [1] of {int}->
	Chandef removed_ch->
	Chandef archive_ch->
	Chandef backup_ch->
	run sync_monitor(backup_ch)->
	run sync_monitor(archive_ch)->
	run sync_monitor(removed_ch)->
	run AnonymousCleanupWaitsForBackupAndArchive85(backup_ch,archive_ch,removed_ch,child_AnonymousCleanupWaitsForBackupAndArchive850)->
	run receiver(child_AnonymousCleanupWaitsForBackupAndArchive850)->
	run AnonymousCleanupWaitsForBackupAndArchive95(backup_ch,archive_ch,removed_ch,child_AnonymousCleanupWaitsForBackupAndArchive951)->
	run receiver(child_AnonymousCleanupWaitsForBackupAndArchive951)->
	run AnonymousCleanupWaitsForBackupAndArchive105(backup_ch,archive_ch,removed_ch,child_AnonymousCleanupWaitsForBackupAndArchive1052)->
	run receiver(child_AnonymousCleanupWaitsForBackupAndArchive1052)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:11:2 */
	:: removed_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:11:2 */->
	:: removed_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:11:2 */ -> 
		removed_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCleanupWaitsForBackupAndArchive85(Chandef backup_ch;Chandef archive_ch;Chandef removed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:8:14 */
	:: backup_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:8:14 */
	:: backup_ch.sync!false -> 
		backup_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:8:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCleanupWaitsForBackupAndArchive95(Chandef backup_ch;Chandef archive_ch;Chandef removed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:9:14 */
	:: archive_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:9:14 */
	:: archive_ch.sync!false -> 
		archive_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:9:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCleanupWaitsForBackupAndArchive105(Chandef backup_ch;Chandef archive_ch;Chandef removed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:10:14 */
	:: backup_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:10:14 */->
	:: backup_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:10:14 */ -> 
		backup_ch.rcving!false
	fi->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:10:24 */
	:: archive_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:10:24 */->
	:: archive_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:10:24 */ -> 
		archive_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:10:35 */
	:: removed_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:10:35 */
	:: removed_ch.sync!false -> 
		removed_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/cleanup.go:10:35 */
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

