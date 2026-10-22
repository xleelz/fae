// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go#L5
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
	chan child_ScratchArchiveRequiresSurvivorAndJournalProof50 = [1] of {int}->
	run ScratchArchiveRequiresSurvivorAndJournalProof5(child_ScratchArchiveRequiresSurvivorAndJournalProof50)->
	child_ScratchArchiveRequiresSurvivorAndJournalProof50?0->
stop_process:skip
}

proctype ScratchArchiveRequiresSurvivorAndJournalProof5(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousScratchArchiveRequiresSurvivorAndJournalProof1162 = [1] of {int}->
	chan child_AnonymousScratchArchiveRequiresSurvivorAndJournalProof1061 = [1] of {int}->
	chan child_AnonymousScratchArchiveRequiresSurvivorAndJournalProof960 = [1] of {int}->
	Chandef deleted_ch->
	Chandef unreferenced_ch->
	Chandef survivor_ch->
	run sync_monitor(survivor_ch)->
	run sync_monitor(unreferenced_ch)->
	run sync_monitor(deleted_ch)->
	run AnonymousScratchArchiveRequiresSurvivorAndJournalProof96(survivor_ch,unreferenced_ch,deleted_ch,child_AnonymousScratchArchiveRequiresSurvivorAndJournalProof960)->
	run receiver(child_AnonymousScratchArchiveRequiresSurvivorAndJournalProof960)->
	run AnonymousScratchArchiveRequiresSurvivorAndJournalProof106(survivor_ch,unreferenced_ch,deleted_ch,child_AnonymousScratchArchiveRequiresSurvivorAndJournalProof1061)->
	run receiver(child_AnonymousScratchArchiveRequiresSurvivorAndJournalProof1061)->
	run AnonymousScratchArchiveRequiresSurvivorAndJournalProof116(survivor_ch,unreferenced_ch,deleted_ch,child_AnonymousScratchArchiveRequiresSurvivorAndJournalProof1162)->
	run receiver(child_AnonymousScratchArchiveRequiresSurvivorAndJournalProof1162)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:12:2 */
	:: deleted_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:12:2 */->
	:: deleted_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:12:2 */ -> 
		deleted_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousScratchArchiveRequiresSurvivorAndJournalProof96(Chandef survivor_ch;Chandef unreferenced_ch;Chandef deleted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:9:14 */
	:: survivor_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:9:14 */
	:: survivor_ch.sync!false -> 
		survivor_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:9:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousScratchArchiveRequiresSurvivorAndJournalProof106(Chandef survivor_ch;Chandef unreferenced_ch;Chandef deleted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:10:14 */
	:: unreferenced_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:10:14 */
	:: unreferenced_ch.sync!false -> 
		unreferenced_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:10:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousScratchArchiveRequiresSurvivorAndJournalProof116(Chandef survivor_ch;Chandef unreferenced_ch;Chandef deleted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:11:14 */
	:: survivor_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:11:14 */->
	:: survivor_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:11:14 */ -> 
		survivor_ch.rcving!false
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:11:26 */
	:: unreferenced_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:11:26 */->
	:: unreferenced_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:11:26 */ -> 
		unreferenced_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:11:42 */
	:: deleted_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:11:42 */
	:: deleted_ch.sync!false -> 
		deleted_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/scratch_archive.go:11:42 */
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

